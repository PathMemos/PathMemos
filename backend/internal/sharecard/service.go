package sharecard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"papafeiji/backend/internal/file"

	"github.com/redis/go-redis/v9"
)

// rendererVersion 参与缓存键：视觉版式变更时递增，旧渲染自然过期。
const rendererVersion = "v2"

const (
	shareCardCacheTTL   = 24 * time.Hour // 内容寻址（键=请求体哈希），TTL 只用于 Redis 内存回收
	shareCardMaxRecords = 50
	shareCardMaxText    = 3000
	shareCardMaxImgRec  = 9
	shareCardMaxImgAll  = 36
)

type Service struct {
	storage *file.Storage
	rdb     *redis.Client
}

func NewService(storage *file.Storage, rdb *redis.Client) *Service {
	return &Service{storage: storage, rdb: rdb}
}

// payloadHash 规范化请求体（剔除空白差异）后做内容寻址哈希。
func payloadHash(req *ShareCardRequest) string {
	b, err := json.Marshal(req)
	if err != nil {
		// 结构体序列化不会失败；兜底格式化。
		b = []byte(fmt.Sprintf("%+v", req))
	}
	h := sha256.Sum256(append([]byte(rendererVersion+":"), b...))
	return hex.EncodeToString(h[:16])
}

// RenderShareCard 渲染并持久化分享卡，返回 {poster, thumb} URL。
// 相同内容（哈希相同）直接命中 Redis 缓存；物理文件按哈希确定性命名，
// 重复渲染覆盖写同路径，不产生孤儿堆积。
func (s *Service) RenderShareCard(ctx context.Context, req *ShareCardRequest) (posterURL, thumbURL string, err error) {
	hash := payloadHash(req)
	cacheKey := "sharecard:" + hash

	if s.rdb != nil {
		if cached, gErr := s.rdb.Get(ctx, cacheKey).Result(); gErr == nil {
			var urls struct {
				Poster string `json:"poster"`
				Thumb  string `json:"thumb"`
			}
			if json.Unmarshal([]byte(cached), &urls) == nil && urls.Poster != "" && urls.Thumb != "" {
				return urls.Poster, urls.Thumb, nil
			}
		}
	}

	poster, thumb, rErr := Render(ctx, req)
	if rErr != nil {
		return "", "", rErr
	}

	// 物理文件确定性命名：内容变→哈希变→新文件；同内容重渲染覆盖写。
	fileKey, storageType, _, sErr := s.storage.SaveSystemWithName(bytes.NewReader(poster), ".jpg", "sc-"+hash)
	if sErr != nil {
		return "", "", fmt.Errorf("save share poster: %w", sErr)
	}
	posterURL, uErr := s.storage.URL(fileKey, storageType)
	if uErr != nil {
		return "", "", fmt.Errorf("share poster url: %w", uErr)
	}
	// 缩略图不建 DB 记录（同邀请图缩略图口径）：确定性命名覆盖写，孤儿由清理任务回收。
	thumbURL = posterURL // 兜底：缩略图失败时回退原图，客户端仍有 ensureThumb 压缩兜底
	if tbKey, tbType, _, tErr := s.storage.SaveSystemWithName(bytes.NewReader(thumb), ".jpg", "sc-"+hash+"-thumb"); tErr != nil {
		slog.WarnContext(ctx, "save share thumb failed", slog.String("hash", hash), slog.Any("error", tErr))
	} else if tu, tuErr := s.storage.URL(tbKey, tbType); tuErr != nil {
		slog.WarnContext(ctx, "share thumb url failed", slog.String("hash", hash), slog.Any("error", tuErr))
	} else {
		thumbURL = tu
	}

	if s.rdb != nil {
		if payload, mErr := json.Marshal(map[string]string{"poster": posterURL, "thumb": thumbURL}); mErr == nil {
			if err := s.rdb.Set(ctx, cacheKey, string(payload), shareCardCacheTTL).Err(); err != nil {
				slog.WarnContext(ctx, "cache share card failed", slog.String("hash", hash), slog.Any("error", err))
			}
		}
	}
	return posterURL, thumbURL, nil
}
