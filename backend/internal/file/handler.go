package file

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"

	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/internal/middleware"
	"papafeiji/backend/internal/vip"
	"papafeiji/backend/pkg/errors"
	"papafeiji/backend/pkg/util"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	uploadFormKey = "files"

	MaxFileSize  = 10 * 1024 * 1024
	MaxTotalSize = 50 * 1024 * 1024
	MaxFiles     = 9
	MaxNameLen   = 255
)

var allowedExts = map[string]string{
	".jpg":  ".jpg",
	".jpeg": ".jpg",
	".png":  ".png",
	".gif":  ".gif",
	".webp": ".webp",
}

type Handler struct {
	router     chi.Router
	pool       *db.Pool
	bgPool     *db.Pool
	rdb        *redis.Client
	storage    *Storage
	cfg        *config.Config
	vipService vip.InfoProvider
}

func NewHandler(router chi.Router, pool, bgPool *db.Pool, rdb *redis.Client, storage *Storage, cfg *config.Config, vipService vip.InfoProvider) *Handler {
	return &Handler{
		router:     router,
		pool:       pool,
		bgPool:     bgPool,
		rdb:        rdb,
		storage:    storage,
		cfg:        cfg,
		vipService: vipService,
	}
}

func (h *Handler) Register() {
	h.router.Delete("/file/{fileId}", h.Delete)
	h.router.Get("/file/download/{fileId}", h.Download)
}

func (h *Handler) RegisterUpload(middlewares ...func(http.Handler) http.Handler) {
	h.router.With(middlewares...).Post("/file/upload", h.Upload)
}

func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	limit := h.storageLimitFor(ctx, userID)
	if err := h.checkStorageLimit(ctx, w, r, userID, limit); err != nil {
		return
	}

	if r.ContentLength > MaxTotalSize {
		middleware.JSONBizError(w, r, errors.BizFileSizeExceeded, "request body too large")
		return
	}

	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid content type")
		return
	}

	// 限制请求体总量：ContentLength==-1（chunked）时前置检查失效，
	// MaxBytesReader 在读取层兜底（+1MB 覆盖 multipart 边界与头部元数据）。
	r.Body = http.MaxBytesReader(w, r.Body, MaxTotalSize+1024*1024)

	reader, err := r.MultipartReader()
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "invalid multipart form")
		return
	}

	var results []map[string]interface{}
	var committed []uploadedPartInfo
	var totalSize int64
	var fileCount int

	// 任一 part 失败时回滚本请求已提交的文件（记录+物理文件+配额），
	// 避免客户端收到失败却已产生入库文件与配额消耗。
	rollback := func() {
		h.rollbackUploadedParts(ctx, userID, committed)
	}

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			rollback()
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "failed to read multipart part")
			return
		}

		if part.FormName() != uploadFormKey {
			_ = part.Close() //nolint:errcheck // multipart close is best-effort
			continue
		}

		if fileCount >= MaxFiles {
			_ = part.Close() //nolint:errcheck // multipart close is best-effort
			rollback()
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, fmt.Sprintf("too many files, max %d", MaxFiles))
			return
		}

		contentType := part.Header.Get("Content-Type")

		res, info, size, err := h.handleUploadPart(ctx, part, contentType, userID, totalSize, limit)
		_ = part.Close() //nolint:errcheck // multipart close is best-effort
		if err != nil {
			rollback()
			switch err {
			case errInvalidFileType:
				middleware.JSONBizError(w, r, errors.BizInvalidFileType, err.Error())
			case errStorageQuotaExceeded:
				middleware.JSONBizError(w, r, errors.BizUserImageStorageLimitExceeded, err.Error())
			case errFileTooLarge:
				middleware.JSONBizError(w, r, errors.BizFileSizeExceeded, err.Error())
			case errTotalSizeExceeded:
				middleware.JSONBizError(w, r, errors.BizFileSizeExceeded, err.Error())
			default:
				middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to upload file")
			}
			return
		}

		totalSize += size

		results = append(results, res)
		committed = append(committed, info)
		fileCount++
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"files": results,
	})
}

// uploadedPartInfo 记录本请求内已提交的文件，用于请求失败时回滚。
type uploadedPartInfo struct {
	fileID      string
	path        string
	storageType string
	size        int64
}

// rollbackUploadedParts 删除记录、物理文件并回退配额，全部 best-effort（记录日志）。
// 回滚多因客户端断开（正是最需要清理的场景）触发，必须脱离请求 ctx，
// 否则 ctx 已取消导致 DB/存储操作全部失败，残留记录+配额+物理文件。
func (h *Handler) rollbackUploadedParts(ctx context.Context, userID string, parts []uploadedPartInfo) {
	bgCtx := context.WithoutCancel(ctx)
	for _, p := range parts {
		if p.fileID == "" {
			continue
		}
		if err := h.pool.Queries().DeleteFile(bgCtx, p.fileID); err != nil {
			slog.ErrorContext(bgCtx, "rollback delete file record failed", slog.String("user_id", userID), slog.String("file_id", p.fileID), slog.Any("error", err))
		}
		if p.path != "" {
			if err := h.storage.DeleteFile(p.path, p.storageType); err != nil {
				slog.ErrorContext(bgCtx, "rollback delete physical file failed", slog.String("user_id", userID), slog.String("file_id", p.fileID), slog.Any("error", err))
			}
		}
		if p.size > 0 {
			if err := h.pool.Queries().DecrementUserImageStorage(bgCtx, sqlc.DecrementUserImageStorageParams{
				ID:                userID,
				ImageStorageBytes: p.size,
			}); err != nil {
				slog.ErrorContext(bgCtx, "rollback decrement storage failed", slog.String("user_id", userID), slog.String("file_id", p.fileID), slog.Any("error", err))
			}
		}
	}
}

var (
	errInvalidFileType      = stderrors.New("invalid file type")
	errFileTooLarge         = stderrors.New("file too large")
	errTotalSizeExceeded    = stderrors.New("total request size exceeded")
	errStorageQuotaExceeded = stderrors.New("user image storage quota exceeded")
)

// storageLimitFor 计算当前用户的存储配额（与 checkStorageLimit 共用，避免重复计算）。
func (h *Handler) storageLimitFor(ctx context.Context, userID string) int64 {
	limit := h.cfg.UserImageStorageLimitBytes
	if h.vipService != nil {
		info, err := h.vipService.GetVIPInfo(ctx, userID)
		if err != nil {
			slog.ErrorContext(ctx, "check vip info for storage limit failed", slog.String("user_id", userID), slog.Any("error", err))
		} else if info.IsVIP {
			limit = h.cfg.UserImageStorageLimitBytesVIP
		}
	}
	return limit
}

func (h *Handler) checkStorageLimit(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string, limit int64) error {
	if limit <= 0 {
		return nil
	}

	used, err := h.pool.Queries().GetUserImageStorageUsage(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "get user image storage usage failed", slog.String("user_id", userID), slog.Any("error", err))
		return nil
	}

	if used >= limit {
		middleware.JSONBizError(w, r, errors.BizUserImageStorageLimitExceeded, "user image storage limit exceeded")
		return stderrors.New("user image storage limit exceeded")
	}

	return nil
}

func (h *Handler) handleUploadPart(ctx context.Context, part *multipart.Part, contentType, userID string, totalSize, storageLimit int64) (map[string]interface{}, uploadedPartInfo, int64, error) {
	ext := extractExt(part.FileName())
	canonicalExt, err := validateFileType(contentType, ext)
	if err != nil {
		return nil, uploadedPartInfo{}, 0, errInvalidFileType
	}

	tmpFile, err := os.CreateTemp("", "pathmemos-upload-*")
	if err != nil {
		return nil, uploadedPartInfo{}, 0, fmt.Errorf("create temp file: %w", err)
	}
	//nolint:errcheck
	defer os.Remove(tmpFile.Name())
	//nolint:errcheck
	defer tmpFile.Close()

	written := int64(0)

	buf := make([]byte, 32*1024)
	for {
		n, err := part.Read(buf)
		if n > 0 {
			if written+int64(n) > MaxFileSize {
				return nil, uploadedPartInfo{}, 0, errFileTooLarge
			}
			written += int64(n)
			if _, werr := tmpFile.Write(buf[:n]); werr != nil {
				return nil, uploadedPartInfo{}, 0, fmt.Errorf("write temp file: %w", werr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, uploadedPartInfo{}, 0, fmt.Errorf("read file part: %w", err)
		}
	}

	if totalSize+written > MaxTotalSize {
		return nil, uploadedPartInfo{}, 0, errTotalSizeExceeded
	}

	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return nil, uploadedPartInfo{}, 0, fmt.Errorf("seek temp file: %w", err)
	}

	fileID, err := util.NewUUID()
	if err != nil {
		return nil, uploadedPartInfo{}, 0, fmt.Errorf("generate file id: %w", err)
	}
	name := sanitizeName(part.FileName())
	metadata := []byte(`{}`)

	useOSS := h.storage.OSSConfigured()
	var key, storageType string
	if useOSS {
		keyUUID, err := util.NewUUID()
		if err != nil {
			return nil, uploadedPartInfo{}, 0, fmt.Errorf("generate oss key id: %w", err)
		}
		key = "uploads/" + time.Now().Format("2006/01") + "/" + keyUUID + canonicalExt
		storageType = "oss"
	} else {
		relPath, _, err := h.storage.Save(tmpFile, canonicalExt)
		if err != nil {
			return nil, uploadedPartInfo{}, 0, fmt.Errorf("save file locally: %w", err)
		}
		key = relPath
		storageType = "local"
	}

	// 文件记录与配额增量同一事务：任一步失败整体回滚，杜绝配额永久漂移。
	err = db.WithTx(ctx, h.pool.Pool(), func(ctx context.Context, q *sqlc.Queries) error {
		if _, err := q.CreateFile(ctx, sqlc.CreateFileParams{
			ID:          fileID,
			CreatedBy:   pgtype.Text{String: userID, Valid: true},
			Path:        key,
			Name:        name,
			Suffix:      strings.TrimPrefix(canonicalExt, "."),
			SizeBytes:   written,
			FileType:    "image",
			StorageType: storageType,
			Metadata:    metadata,
		}); err != nil {
			return fmt.Errorf("create file record: %w", err)
		}
		// 条件原子扣减——超限时 0 行，杜绝并发 check-then-act 竞态。
		rows, err := q.IncrementUserImageStorage(ctx, sqlc.IncrementUserImageStorageParams{
			ID:                userID,
			ImageStorageBytes: written,
			StorageLimit:      storageLimit,
		})
		if err != nil {
			return fmt.Errorf("increment user image storage: %w", err)
		}
		if rows == 0 {
			return errStorageQuotaExceeded
		}
		return nil
	})
	if err != nil {
		if !useOSS {
			_ = h.storage.DeleteFile(key, storageType) //nolint:errcheck
		}
		return nil, uploadedPartInfo{}, 0, err
	}

	if useOSS {
		if _, uploadErr := h.storage.SaveToOSSWithKey(tmpFile, key, written); uploadErr != nil {
			// 内联回滚同样必须脱离请求 ctx（02g F-1）：客户端断开后请求 ctx 已取消，
			// 沿用它会让 DB/存储回滚全部失败，残留记录+配额+对象三态。
			rollbackCtx := context.WithoutCancel(ctx)
			_ = h.pool.Queries().DeleteFile(rollbackCtx, fileID) //nolint:errcheck // rollback cleanup is best-effort
			_ = h.storage.DeleteFile(key, "oss")                 //nolint:errcheck // rollback cleanup is best-effort
			if decErr := h.pool.Queries().DecrementUserImageStorage(rollbackCtx, sqlc.DecrementUserImageStorageParams{
				ID:                userID,
				ImageStorageBytes: written,
			}); decErr != nil {
				slog.ErrorContext(rollbackCtx, "rollback decrement user image storage failed",
					slog.String("user_id", userID),
					slog.String("file_id", fileID),
					slog.Int64("size", written),
					slog.Any("error", decErr))
			}
			return nil, uploadedPartInfo{}, 0, fmt.Errorf("upload file to oss: %w", uploadErr)
		}
	}

	url, urlErr := h.storage.URL(key, storageType)
	if urlErr != nil {
		// 与 OSS 上传失败路径一致：文件记录/配额已提交，URL 构造失败时须内联清理，
		// 否则 DB 记录、配额增量、物理文件全部残留（仅靠 7 天孤儿清理兜底）。
		rollbackCtx := context.WithoutCancel(ctx)
		_ = h.pool.Queries().DeleteFile(rollbackCtx, fileID) //nolint:errcheck
		_ = h.storage.DeleteFile(key, storageType)           //nolint:errcheck
		if decErr := h.pool.Queries().DecrementUserImageStorage(rollbackCtx, sqlc.DecrementUserImageStorageParams{
			ID:                userID,
			ImageStorageBytes: written,
		}); decErr != nil {
			slog.ErrorContext(rollbackCtx, "rollback decrement user image storage failed",
				slog.String("user_id", userID),
				slog.String("file_id", fileID),
				slog.Int64("size", written),
				slog.Any("error", decErr))
		}
		return nil, uploadedPartInfo{}, 0, fmt.Errorf("get file url: %w", urlErr)
	}
	return map[string]interface{}{
		"fileId": fileID,
		"url":    url,
	}, uploadedPartInfo{fileID: fileID, path: key, storageType: storageType, size: written}, written, nil
}

func sanitizeName(name string) string {
	if len(name) <= MaxNameLen {
		return name
	}
	truncated := name[:MaxNameLen]
	for len(truncated) > 0 && !utf8.RuneStart(truncated[len(truncated)-1]) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

func extractExt(name string) string {
	idx := strings.LastIndex(name, ".")
	if idx < 0 {
		return ""
	}
	return strings.ToLower(name[idx:])
}

func validateFileType(contentType, ext string) (string, error) {
	if contentType == "" || !strings.HasPrefix(contentType, "image/") {
		return "", fmt.Errorf("invalid content type: %s", contentType)
	}
	ext = strings.ToLower(ext)
	canonical, ok := allowedExts[ext]
	if !ok {
		return "", fmt.Errorf("unsupported file extension: %s", ext)
	}
	return canonical, nil
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)
	fileID := chi.URLParam(r, "fileId")

	if fileID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "fileId is required")
		return
	}

	if err := DeleteFile(ctx, h.pool, h.storage, fileID, userID); err != nil {

		if stderrors.Is(err, ErrNotFileOwner) {
			middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, "file does not belong to user")
			return
		}
		if stderrors.Is(err, ErrSystemFileDelete) {
			middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, "system file cannot be deleted")
			return
		}
		if stderrors.Is(err, ErrFileInUse) {
			middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "file is still in use")
			return
		}
		if stderrors.Is(err, ErrFileNotFound) {
			middleware.JSONError(w, r, http.StatusNotFound, errors.CodeNotFound, "file not found")
			return
		}
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to delete file")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)
	fileID := chi.URLParam(r, "fileId")

	if fileID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, errors.CodeBadRequest, "fileId is required")
		return
	}

	file, err := h.pool.Queries().GetFileByID(ctx, fileID)
	if err != nil {
		if stderrors.Is(err, pgx.ErrNoRows) {
			middleware.JSONError(w, r, http.StatusNotFound, errors.CodeNotFound, "file not found")
			return
		}

		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get file")
		return
	}

	if file.FileType == "system" {
		var meta struct {
			FamilyID string `json:"family_id"`
		}
		if err := json.Unmarshal(file.Metadata, &meta); err != nil || meta.FamilyID == "" {
			middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, "system file access denied")
			return
		}
		user, err := h.pool.Queries().GetUserByID(ctx, userID)
		if err != nil || !user.CurrentFamilyID.Valid || user.CurrentFamilyID.String != meta.FamilyID {
			middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, "system file access denied")
			return
		}
	} else {
		if !file.CreatedBy.Valid || file.CreatedBy.String == "" {
			middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, "file access denied")
			return
		}
		if file.CreatedBy.String != userID {

			allowed, err := h.isFileAccessible(ctx, userID, file.CreatedBy.String)
			if err != nil {

				middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to check file access")
				return
			}
			if !allowed {
				middleware.JSONError(w, r, http.StatusForbidden, errors.CodeForbidden, "file does not belong to user")
				return
			}
		}
	}

	url, urlErr := h.storage.URL(file.Path, file.StorageType)
	if urlErr != nil {
		slog.ErrorContext(ctx, "construct file url failed", slog.Any("error", urlErr), slog.String("fileId", fileID), slog.String("path", file.Path))
		middleware.JSONError(w, r, http.StatusInternalServerError, errors.CodeInternalError, "failed to get file url")
		return
	}
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"url": url,
	})
}

func (h *Handler) isFileAccessible(ctx context.Context, userID, creatorID string) (bool, error) {
	u, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("get current user: %w", err)
	}
	c, err := h.pool.Queries().GetUserByID(ctx, creatorID)
	if err != nil {
		return false, fmt.Errorf("get creator user: %w", err)
	}
	if !u.CurrentFamilyID.Valid || !c.CurrentFamilyID.Valid {
		return false, nil
	}
	return u.CurrentFamilyID.String == c.CurrentFamilyID.String, nil
}
