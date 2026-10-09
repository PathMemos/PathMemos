package sharecard

import (
	"net/http"
	"strings"
	"unicode/utf8"

	"papafeiji/backend/internal/middleware"
	pkgerrors "papafeiji/backend/pkg/errors"

	"github.com/go-chi/chi/v5"
)

const shareCardBodyLimit = 512 * 1024 // 50 条 × 3000 字 UTF-8 的最坏包体

type Handler struct {
	router chi.Router
	svc    *Service
}

func NewHandler(router chi.Router, svc *Service) *Handler {
	return &Handler{router: router, svc: svc}
}

// Register 注册 POST /diary/share-card（会话保护组内调用；限流/超时由 wiring 传入）。
func (h *Handler) Register(mws ...func(http.Handler) http.Handler) {
	r := h.router
	for i := len(mws) - 1; i >= 0; i-- {
		r = r.With(mws[i])
	}
	r.Post("/diary/share-card", h.Create)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req ShareCardRequest
	if err := middleware.ReadJSONBody(w, r, &req, shareCardBodyLimit); err != nil {
		middleware.JSONBodyError(w, r, err)
		return
	}
	if msg := validate(&req); msg != "" {
		middleware.JSONError(w, r, http.StatusBadRequest, pkgerrors.CodeBadRequest, msg)
		return
	}

	posterURL, thumbURL, err := h.svc.RenderShareCard(ctx, &req)
	if err != nil {
		middleware.JSONError(w, r, http.StatusInternalServerError, pkgerrors.CodeInternalError, "render share card failed")
		return
	}
	middleware.JSON(w, r, http.StatusOK, map[string]string{"poster": posterURL, "thumb": thumbURL})
}

// validate 契约上限校验：与客户端前置裁剪一致（50 条/3000 字/9 图每条）。
func validate(req *ShareCardRequest) string {
	req.Header.Title = strings.TrimSpace(req.Header.Title)
	req.Header.Subtitle = strings.TrimSpace(req.Header.Subtitle)
	if utf8.RuneCountInString(req.Header.Title) > 100 {
		return "header.title too long"
	}
	if utf8.RuneCountInString(req.Header.Subtitle) > 100 {
		return "header.subtitle too long"
	}
	if utf8.RuneCountInString(req.Brand) > 30 {
		return "brand too long"
	}
	if utf8.RuneCountInString(req.CountLabel) > 60 {
		return "countLabel too long"
	}
	if len(req.MemberPill) > 10 {
		return "too many memberPills"
	}
	for _, p := range req.MemberPill {
		if utf8.RuneCountInString(p) > 30 {
			return "memberPill too long"
		}
	}
	if !isHTTPURL(req.CoverImg) {
		req.CoverImg = ""
	}
	if req.QR.URL != "" && !isHTTPURL(req.QR.URL) {
		return "qr.url invalid"
	}
	if utf8.RuneCountInString(req.Slogan) > 60 || utf8.RuneCountInString(req.SubSlogan) > 80 {
		return "slogan too long"
	}
	if utf8.RuneCountInString(req.EmptyTitle) > 60 || utf8.RuneCountInString(req.EmptyTip) > 80 {
		return "empty fields too long"
	}
	if len(req.Records) > shareCardMaxRecords {
		return "too many records"
	}
	totalImgs := 0
	for i := range req.Records {
		rec := &req.Records[i]
		rec.Text = strings.TrimSpace(rec.Text)
		if utf8.RuneCountInString(rec.Text) > shareCardMaxText {
			return "record text too long"
		}
		if utf8.RuneCountInString(rec.TimeText) > 20 || utf8.RuneCountInString(rec.MemberName) > 30 || utf8.RuneCountInString(rec.Address) > 120 {
			return "record field too long"
		}
		if len(rec.Images) > shareCardMaxImgRec {
			rec.Images = rec.Images[:shareCardMaxImgRec]
		}
		kept := rec.Images[:0]
		for _, u := range rec.Images {
			if isHTTPURL(u) {
				kept = append(kept, u)
			}
		}
		rec.Images = kept
		totalImgs += len(rec.Images)
		if totalImgs > shareCardMaxImgAll {
			return "too many images"
		}
	}
	return ""
}

func isHTTPURL(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}
