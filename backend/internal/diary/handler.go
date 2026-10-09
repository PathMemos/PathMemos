package diary

import (
	"fmt"

	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"errors"
	"log/slog"
	"unicode/utf8"

	"papafeiji/backend/internal/db"
	"papafeiji/backend/internal/middleware"
	"papafeiji/backend/internal/vip"
	pkgerrors "papafeiji/backend/pkg/errors"
	"papafeiji/backend/pkg/timeutil"
	"papafeiji/backend/pkg/util"
	"papafeiji/backend/pkg/validator"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Handler struct {
	router     chi.Router
	pool       *db.Pool
	vipService vip.InfoProvider
	svc        *Service
}

func NewHandler(router chi.Router, pool *db.Pool, vipService vip.InfoProvider, svc *Service) *Handler {
	return &Handler{
		router:     router,
		pool:       pool,
		vipService: vipService,
		svc:        svc,
	}
}

func (h *Handler) Register() {
	h.router.Get("/diary/info", h.ListInfo)
	h.router.Get("/diary/stats", h.GetStats)
	h.router.Post("/diary/info/dates", h.ListInfoDates)
	h.router.Put("/diary/info", h.UpdateInfoCover)
	h.router.Delete("/diary/info", h.DeleteInfo)

	h.router.Get("/diary/details", h.GetDetails)
	h.router.Post("/diary/details", h.CreateEntry)
	h.router.Put("/diary/details", h.UpdateEntry)
	h.router.Delete("/diary/details", h.DeleteEntry)
	h.router.Post("/diary/details/auto", h.CreateAutoEntry)
	h.router.Post("/diary/details/memory", h.CreateMemory)
	h.router.Put("/diary/details/memory", h.UpdateMemory)
	h.router.Delete("/diary/details/memory", h.DeleteMemory)

	h.router.Get("/diary/cover-url", h.GetCoverURL)
}

func (h *Handler) ListInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	cursorDate := r.URL.Query().Get("cursorDate")
	if cursorDate == "" {
		cursorDate = "9999-12-31"
	}
	if _, err := time.Parse(dateFormat, cursorDate); err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid cursorDate")
		return
	}

	size := parseIntDefault(r.URL.Query().Get("size"), 10)
	if size < 1 {
		size = 10
	}
	if size > 20 {
		size = 20
	}

	cards, nextCursor, err := h.svc.ListInfoCards(ctx, userID, cursorDate, size)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list diary info", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to list diary info")
		return
	}

	middleware.JSONWithPagination(w, r, http.StatusOK, cards, nextCursor, len(cards))
}

func (h *Handler) ListInfoDates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		Dates []string `json:"dates"`
	}
	if !readDiaryJSONBody(w, r, &req, 64*1024) {
		return
	}

	if len(req.Dates) > 31 {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "too many dates")
		return
	}
	if len(req.Dates) == 0 {
		middleware.JSON(w, r, http.StatusOK, []interface{}{})
		return
	}

	seen := make(map[string]bool)
	var dates []string
	for _, d := range req.Dates {
		normalized, err := normalizeDate(d)
		if err != nil {
			middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid date format")
			return
		}
		if !seen[normalized] {
			seen[normalized] = true
			dates = append(dates, normalized)
		}
	}
	sort.Strings(dates)

	cards, err := h.svc.ListInfoCardsByDates(ctx, userID, dates)
	if err != nil {
		slog.ErrorContext(ctx, "failed to list diary info by dates", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to list diary info")
		return
	}

	middleware.JSON(w, r, http.StatusOK, cards)
}

func (h *Handler) GetStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	stats, err := h.svc.GetStats(ctx, userID)
	if err != nil {
		slog.ErrorContext(ctx, "failed to get diary stats", slog.String("user_id", userID), slog.Any("error", err))
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to get diary stats")
		return
	}

	middleware.JSON(w, r, http.StatusOK, stats)
}

func (h *Handler) UpdateInfoCover(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		ID         string  `json:"id"`
		CoverImage *string `json:"coverImage"`
	}
	if !readDiaryJSONBody(w, r, &req, 64*1024) {
		return
	}

	familyID, recordDate, err := parseVirtualID(req.ID)
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid diary id")
		return
	}

	var fileID pgtype.Text
	if req.CoverImage != nil {
		fileID = pgtype.Text{String: *req.CoverImage, Valid: true}
	}

	if err := h.svc.UpdateCover(ctx, userID, familyID, recordDate, fileID); err != nil {
		writeDiaryServiceError(w, r, err, "update cover")
		return
	}

	// UpdateCover 已在锁内完成封面状态评估与降级，handler 无需再补偿刷新。
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) DeleteInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)
	id := r.URL.Query().Get("id")

	familyID, recordDate, err := parseVirtualID(id)
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid diary id")
		return
	}

	if err := h.svc.DeleteDiary(ctx, userID, familyID, recordDate); err != nil {
		if errors.Is(err, ErrFamilyMismatch) {
			middleware.JSONError(w, r, http.StatusForbidden, pkgerrors.CodeForbidden, "diary does not belong to current family")
			return
		}
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to delete diary")
		return
	}

	h.svc.InvalidateFamilySummary(ctx, familyID)
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) GetDetails(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	diaryID := r.URL.Query().Get("diaryId")
	memberUserID := r.URL.Query().Get("memberUserId")
	page := parseIntDefault(r.URL.Query().Get("page"), 1)
	if page < 1 {
		page = 1
	}
	size := parseIntDefault(r.URL.Query().Get("size"), 20)
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}

	familyID, recordDate, err := parseVirtualID(diaryID)
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid diary id")
		return
	}

	resp, err := h.svc.GetDetails(ctx, userID, familyID, recordDate, memberUserID, page, size)
	if err != nil {
		if errors.Is(err, ErrFamilyMismatch) {
			middleware.JSONError(w, r, http.StatusForbidden, pkgerrors.CodeForbidden, "diary does not belong to current family")
			return
		}
		if errors.Is(err, ErrMemberNotFound) {
			middleware.JSONError(w, r, http.StatusNotFound, pkgerrors.CodeNotFound, "member not found")
			return
		}
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to get diary details")
		return
	}

	entries, ok := resp["data"].([]map[string]interface{})
	if !ok {
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "invalid diary details response")
		return
	}
	extra, _ := resp["extra"].(map[string]interface{})
	// 服务端已算得条目总数，透传 count 供前端分页判断。
	count, _ := resp["count"].(int64)
	middleware.JSONWithExtraAndCount(w, r, http.StatusOK, entries, extra, int(count))
}

func readDiaryJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes int64) bool {
	if err := middleware.ReadJSONBody(w, r, dst, maxBytes); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			middleware.JSONError(w, r, http.StatusRequestEntityTooLarge, pkgerrors.CodeRequestEntityTooLarge, "request body too large")
		} else {
			middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid request body")
		}
		return false
	}
	return true
}

func writeEntryValidationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errTextTooLong) {
		middleware.JSONBizError(w, r, pkgerrors.BizTextTooLong, err.Error())
	} else if errors.Is(err, errInvalidColor) {
		middleware.JSONBizError(w, r, pkgerrors.BizInvalidColorFormat, err.Error())
	} else if errors.Is(err, errInvalidCoords) {
		middleware.JSONBizError(w, r, pkgerrors.BizInvalidCoordinates, err.Error())
	} else if errors.Is(err, errTooManyImages) {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, err.Error())
	} else {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, err.Error())
	}
}

func writeDiaryServiceError(w http.ResponseWriter, r *http.Request, err error, action string) {
	switch {
	case errors.Is(err, ErrEntryNotFound):
		middleware.JSONError(w, r, http.StatusNotFound, pkgerrors.CodeNotFound, err.Error())
	case errors.Is(err, ErrFileNotFound) || errors.Is(err, ErrNotFileOwner) || errors.Is(err, ErrFileNotImage) || errors.Is(err, ErrCoverImageNotFromDiary):
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, err.Error())
	case errors.Is(err, ErrFamilyMismatch):
		middleware.JSONError(w, r, http.StatusForbidden, pkgerrors.CodeForbidden, "diary does not belong to current family")
	case errors.Is(err, ErrPermissionDenied):
		middleware.JSONError(w, r, http.StatusForbidden, pkgerrors.CodeForbidden, err.Error())
	case errors.Is(err, ErrCoverUpdateInProgress):
		middleware.JSONBizError(w, r, pkgerrors.BizOperationInProgress, "cover update in progress")
	default:
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to "+action)
	}
}

func (h *Handler) CreateEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req entryRequest
	if !readDiaryJSONBody(w, r, &req, 64*1024) {
		return
	}

	if err := validateEntryRequest(&req); err != nil {
		writeEntryValidationError(w, r, err)
		return
	}

	entryID, familyID, _, card, err := h.svc.CreateEntry(ctx, userID, &req)
	if err != nil {
		writeDiaryServiceError(w, r, err, "create entry")
		return
	}

	h.svc.InvalidateFamilySummary(ctx, familyID)
	resp := map[string]interface{}{"id": entryID}
	if card != nil {
		resp["card"] = card
	}
	middleware.JSON(w, r, http.StatusOK, resp)
}

func (h *Handler) UpdateEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req entryRequest
	if !readDiaryJSONBody(w, r, &req, 64*1024) {
		return
	}
	if req.ID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "id is required")
		return
	}

	if err := validateEntryRequest(&req); err != nil {
		writeEntryValidationError(w, r, err)
		return
	}

	familyID, _, card, err := h.svc.UpdateEntry(ctx, userID, &req)
	if err != nil {
		writeDiaryServiceError(w, r, err, "update entry")
		return
	}

	h.svc.InvalidateFamilySummary(ctx, familyID)
	resp := map[string]interface{}{}
	if card != nil {
		resp["card"] = card
	}
	middleware.JSON(w, r, http.StatusOK, resp)
}

func (h *Handler) DeleteEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)
	entryID := r.URL.Query().Get("id")

	if entryID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "id is required")
		return
	}

	familyID, _, err := h.svc.DeleteEntry(ctx, userID, entryID)
	if err != nil {
		if errors.Is(err, ErrEntryNotFound) {
			middleware.JSONError(w, r, http.StatusNotFound, pkgerrors.CodeNotFound, err.Error())
		} else if errors.Is(err, ErrPermissionDenied) {
			middleware.JSONError(w, r, http.StatusForbidden, pkgerrors.CodeForbidden, err.Error())
		} else {
			middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to delete entry")
		}
		return
	}

	h.svc.InvalidateFamilySummary(ctx, familyID)
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) CreateMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		Title      string `json:"title"`
		Content    string `json:"content"`
		RecordTime string `json:"recordTime"`
	}
	if !readDiaryJSONBody(w, r, &req, 64*1024) {
		return
	}

	titleLen := utf8.RuneCountInString(strings.TrimSpace(req.Title))
	if titleLen == 0 || titleLen > 50 {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "标题需为 1-50 个字")
		return
	}
	contentLen := utf8.RuneCountInString(strings.TrimSpace(req.Content))
	if contentLen == 0 || contentLen > 10000 {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "内容需为 1-10000 个字")
		return
	}

	recordTime, err := time.Parse(time.RFC3339Nano, req.RecordTime)
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid recordTime")
		return
	}

	id, err := h.svc.CreateMemory(ctx, userID, strings.TrimSpace(req.Title), strings.TrimSpace(req.Content), recordTime)
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to create memory")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"id": id})
}

func (h *Handler) UpdateMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	var req struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		Content    string `json:"content"`
		RecordTime string `json:"recordTime"`
	}
	if !readDiaryJSONBody(w, r, &req, 64*1024) {
		return
	}
	if req.ID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "id is required")
		return
	}
	titleLen := utf8.RuneCountInString(strings.TrimSpace(req.Title))
	if titleLen == 0 || titleLen > 50 {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "标题需为 1-50 个字")
		return
	}
	contentLen := utf8.RuneCountInString(strings.TrimSpace(req.Content))
	if contentLen == 0 || contentLen > 10000 {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "内容需为 1-10000 个字")
		return
	}

	recordTime, err := time.Parse(time.RFC3339Nano, req.RecordTime)
	if err != nil {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "invalid recordTime")
		return
	}

	if err := h.svc.UpdateMemory(ctx, userID, req.ID, strings.TrimSpace(req.Title), strings.TrimSpace(req.Content), recordTime); err != nil {
		if errors.Is(err, ErrMemoryNotFound) {
			middleware.JSONError(w, r, http.StatusNotFound, pkgerrors.CodeNotFound, err.Error())
			return
		}

		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to update memory")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) DeleteMemory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)
	memoryID := r.URL.Query().Get("id")

	if memoryID == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "id is required")
		return
	}

	if err := h.svc.DeleteMemory(ctx, userID, memoryID); err != nil {
		if errors.Is(err, ErrMemoryNotFound) {
			middleware.JSONError(w, r, http.StatusNotFound, pkgerrors.CodeNotFound, err.Error())
			return
		}
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to delete memory")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{})
}

func (h *Handler) CreateAutoEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	info, err := h.vipService.GetVIPInfo(ctx, userID)
	if err != nil {

		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to check vip status")
		return
	}
	if !info.IsVIP {
		middleware.JSONBizError(w, r, pkgerrors.BizNotVip, "not vip")
		return
	}

	var req struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	}
	if !readDiaryJSONBody(w, r, &req, 64*1024) {
		return
	}
	if err := validator.ValidateCoordinates(req.Lat, req.Lon); err != nil {
		middleware.JSONBizError(w, r, pkgerrors.BizInvalidCoordinates, err.Error())
		return
	}

	entryID, familyID, _, err := h.svc.CreateAutoEntry(ctx, userID, req.Lat, req.Lon)
	if err != nil {
		if errors.Is(err, ErrDailyReverseQuotaExceeded) {
			middleware.JSONBizError(w, r, pkgerrors.BizRateLimited, "daily reverse geocode quota exceeded")
			return
		}
		if errors.Is(err, ErrAutoEntryInProgress) {
			middleware.JSONBizError(w, r, pkgerrors.BizOperationInProgress, "auto entry in progress")
			return
		}
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to create auto entry")
		return
	}

	h.svc.InvalidateFamilySummary(ctx, familyID)
	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{"id": entryID})
}

func parseVirtualID(id string) (familyID, recordDate string, err error) {
	parts := strings.Split(id, ":")
	if len(parts) != 4 || parts[0] != "family" || parts[2] != "date" {
		return "", "", fmt.Errorf("invalid virtual id")
	}
	if _, err := time.Parse(dateFormat, parts[3]); err != nil {
		return "", "", fmt.Errorf("invalid date")
	}
	return parts[1], parts[3], nil
}

func makeVirtualID(familyID, recordDate string) string {
	return fmt.Sprintf("family:%s:date:%s", familyID, recordDate)
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func todayShanghai() string {
	return timeutil.NowShanghai().Format(dateFormat)
}

type entryRequest struct {
	ID            string   `json:"id"`
	Text          *string  `json:"text"`
	ImageIDs      []string `json:"imageIds"`
	Lat           *float64 `json:"lat"`
	Lon           *float64 `json:"lon"`
	Address       *string  `json:"address"`
	DetailAddress *string  `json:"detailAddress"`
	RecordTime    *string  `json:"recordTime"`
	Sort          *int32   `json:"sort"`
	Color         *string  `json:"color"`
}

const maxImagesPerEntry = 9

var (
	errTextTooLong    = errors.New("text too long")
	errInvalidColor   = errors.New("invalid color")
	errInvalidCoords  = errors.New("invalid coordinates")
	errAddressTooLong = errors.New("address too long")
	errTooManyImages  = errors.New("too many images")
)

func validateEntryRequest(req *entryRequest) error {
	if len(req.ImageIDs) > maxImagesPerEntry {
		return errTooManyImages
	}

	if req.Text != nil && utf8CodePoints(*req.Text) > 10000 {
		return errTextTooLong
	}

	if req.Color != nil && *req.Color != "" {
		if err := validator.ValidateColor(*req.Color); err != nil {
			return errInvalidColor
		}
	}

	if req.Lat != nil || req.Lon != nil {
		if req.Lat == nil || req.Lon == nil {
			return errInvalidCoords
		}
		if err := validator.ValidateCoordinates(*req.Lat, *req.Lon); err != nil {
			return errInvalidCoords
		}
	}

	if req.Address != nil && utf8.RuneCountInString(*req.Address) > 500 {
		return errAddressTooLong
	}
	if req.DetailAddress != nil && utf8.RuneCountInString(*req.DetailAddress) > 500 {
		return errAddressTooLong
	}

	if req.RecordTime != nil && *req.RecordTime != "" {
		if _, err := time.Parse(time.RFC3339, *req.RecordTime); err != nil {
			return fmt.Errorf("invalid recordTime")
		}
	}

	return nil
}

func utf8CodePoints(s string) int {
	var count int
	for range s {
		count++
	}
	return count
}

func (h *Handler) GetCoverURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserID(ctx)

	familyID := r.URL.Query().Get("familyId")
	recordDate := r.URL.Query().Get("recordDate")
	if familyID == "" || recordDate == "" {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, "familyId and recordDate are required")
		return
	}

	user, err := h.pool.Queries().GetUserByID(ctx, userID)
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to get user")
		return
	}
	if util.ToString(user.CurrentFamilyID) != familyID {
		middleware.JSONError(w, r, http.StatusForbidden, pkgerrors.CodeForbidden, "not a member of this family")
		return
	}

	coverURL, err := h.svc.GetCoverURL(ctx, familyID, recordDate)
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "failed to get cover")
		return
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"coverImg": coverURL,
	})
}
