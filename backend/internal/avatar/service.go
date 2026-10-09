// Package avatar provides related functionality.
package avatar

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	stdDraw "image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/file"
	"papafeiji/backend/internal/pkg/safe"
	"papafeiji/backend/pkg/util"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/image/draw"
)

const (
	TrajectoryMarkerSize   = 75
	TrajectoryMarkerBorder = 2
	MaxAvatarDownloadSize  = 5 * 1024 * 1024
	GenerateMarkerTimeout  = 30 * time.Second
)

type Service struct {
	pool    *db.Pool
	storage *file.Storage
}

func NewService(pool *db.Pool, storage *file.Storage) *Service {
	return &Service{
		pool:    pool,
		storage: storage,
	}
}

func (s *Service) GenerateMarker(ctx context.Context, userID, avatarURL string) (string, error) {
	if avatarURL == "" {
		return "", nil
	}

	// 不为此加分布式锁：连续快速换头像时，最坏结果只是产生孤儿 marker 文件，
	// 由后台清理任务回收；避免为极低概率事件增加复杂锁逻辑（AGENTS.md 简单优先）。
	return s.generateMarkerOnce(ctx, userID, avatarURL)
}

func (s *Service) generateMarkerOnce(ctx context.Context, userID, avatarURL string) (string, error) {
	imageBytes, err := s.downloadAvatar(ctx, avatarURL)
	if err != nil {
		return "", fmt.Errorf("download avatar: %w", err)
	}

	var buf bytes.Buffer
	if err := s.processTrajectoryMarker(imageBytes, &buf); err != nil {
		return "", fmt.Errorf("process marker: %w", err)
	}

	relPath, storageType, size, fileName, err := s.saveMarkerWithShortCode(ctx, &buf)
	if err != nil {
		return "", fmt.Errorf("save marker: %w", err)
	}

	fileID, err := util.NewUUID()
	if err != nil {
		return "", fmt.Errorf("generate marker file id: %w", err)
	}
	if _, err := s.pool.Queries().CreateFile(ctx, sqlc.CreateFileParams{
		ID:          fileID,
		CreatedBy:   pgtype.Text{String: userID, Valid: true},
		Path:        relPath,
		Name:        fileName + ".png",
		Suffix:      "png",
		SizeBytes:   size,
		FileType:    "system",
		StorageType: storageType,
		Metadata:    []byte(`{}`),
	}); err != nil {
		return "", fmt.Errorf("record marker file: %w", err)
	}

	// 先读取旧 marker，DB 更新成功后再异步清理物理文件，避免删除仍被引用的文件。
	oldMarkers, err := s.pool.Queries().GetUserAvatarMarkersByIDs(ctx, []string{userID})
	var oldPath, oldStorageType string
	if err != nil {
		slog.WarnContext(ctx, "get old avatar marker failed, skipping cleanup", slog.String("user_id", userID), slog.Any("error", err))
	} else if len(oldMarkers) > 0 {
		oldPath = oldMarkers[0].MarkerPath
		oldStorageType = oldMarkers[0].StorageType
	}

	if err := s.pool.Queries().UpsertUserAvatarMarker(ctx, sqlc.UpsertUserAvatarMarkerParams{
		UserID:      userID,
		MarkerPath:  relPath,
		StorageType: storageType,
	}); err != nil {
		return "", fmt.Errorf("save marker record: %w", err)
	}

	if oldPath != "" && oldPath != relPath {
		safe.Go(ctx, nil, func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := s.storage.DeleteFile(oldPath, oldStorageType); err != nil {
				slog.WarnContext(bgCtx, "delete old avatar marker failed",
					slog.String("user_id", userID),
					slog.String("path", oldPath),
					slog.Any("error", err))
			}
		})
	}

	url, err := s.storage.URL(relPath, storageType)
	if err != nil {
		return "", fmt.Errorf("get marker url: %w", err)
	}
	return url, nil
}

func (s *Service) saveMarkerWithShortCode(ctx context.Context, buf *bytes.Buffer) (string, string, int64, string, error) {
	ext := ".png"
	fileName, err := util.NewUUID()
	if err != nil {
		return "", "", 0, "", fmt.Errorf("generate marker file name: %w", err)
	}
	relPath, storageType, size, err := s.storage.SaveSystemWithName(buf, ext, fileName)
	if err != nil {
		return "", "", 0, "", err
	}
	return relPath, storageType, size, fileName, nil
}

func (s *Service) downloadAvatar(ctx context.Context, rawURL string) ([]byte, error) {
	if !util.IsPublicHTTPSURL(rawURL) {
		return nil, fmt.Errorf("avatar url is not a public https url")
	}
	// 图床/CDN 挂起时必须有明确时限，避免核心换头像路径无限阻塞。
	ctx, cancel := context.WithTimeout(ctx, GenerateMarkerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := *config.HTTPClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !util.IsPublicHTTPSURL(req.URL.String()) {
			return fmt.Errorf("redirect url is not a public https url")
		}
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("avatar download status %d", resp.StatusCode)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "image") {
		return nil, fmt.Errorf("avatar content type is not image: %s", contentType)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxAvatarDownloadSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxAvatarDownloadSize {
		return nil, fmt.Errorf("avatar too large")
	}
	return data, nil
}

const MaxPixelDimension = 4096

func (s *Service) processTrajectoryMarker(imageBytes []byte, w io.Writer) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(imageBytes))
	if err != nil {
		return fmt.Errorf("decode avatar config: %w", err)
	}
	if cfg.Width > MaxPixelDimension || cfg.Height > MaxPixelDimension {
		return fmt.Errorf("avatar dimensions too large: %dx%d (max %d)", cfg.Width, cfg.Height, MaxPixelDimension)
	}

	img, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return fmt.Errorf("decode avatar image: %w", err)
	}

	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	stdDraw.Draw(rgba, bounds, img, bounds.Min, stdDraw.Src)

	size := TrajectoryMarkerSize
	thumb := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.CatmullRom.Scale(thumb, thumb.Bounds(), rgba, bounds, stdDraw.Over, nil)

	totalSize := size + TrajectoryMarkerBorder*2
	result := image.NewRGBA(image.Rect(0, 0, totalSize, totalSize))

	white := color.RGBA{255, 255, 255, 255}
	center := totalSize / 2
	radius := totalSize / 2
	for y := 0; y < totalSize; y++ {
		for x := 0; x < totalSize; x++ {
			dx := x - center
			dy := y - center
			if dx*dx+dy*dy <= radius*radius {
				result.Set(x, y, white)
			}
		}
	}

	avatarCenter := size / 2
	avatarRadius := size / 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := x - avatarCenter
			dy := y - avatarCenter
			if dx*dx+dy*dy <= avatarRadius*avatarRadius {
				result.Set(x+TrajectoryMarkerBorder, y+TrajectoryMarkerBorder, thumb.At(x, y))
			}
		}
	}

	if err := png.Encode(w, result); err != nil {
		return fmt.Errorf("encode marker png: %w", err)
	}
	return nil
}
