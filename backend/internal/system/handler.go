// Package system provides public system-wide endpoints.
package system

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/middleware"
)

type Handler struct {
	cfg *config.Config
}

func NewHandler(router chi.Router, cfg *config.Config) *Handler {
	h := &Handler{cfg: cfg}
	router.Get("/system/config", h.GetConfig)
	return h
}

func (h *Handler) GetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg

	features := map[string]bool{
		"ai":  cfg.AIAPIKey != "",
		"mcp": cfg.MCPEnabled,
	}

	if cfg.DeploymentMode == "open" {
		// 开源版：微信支付回调指向 SaaS，付费购买关闭；免费 VIP 默认可用（FREE_VIP_ENABLED 可关）。
		features["payment"] = false
		features["freeVip"] = cfg.FreeVipEnabled
		features["wxmp"] = false
	} else {
		features["payment"] = cfg.WechatVirtualOfferID != "" &&
			cfg.WechatVirtualAppKeyProd != "" &&
			cfg.WechatVirtualAppKeySandbox != ""
		features["freeVip"] = cfg.FreeVipEnabled
		features["wxmp"] = cfg.WechatMsgToken != ""
	}

	middleware.JSON(w, r, http.StatusOK, map[string]interface{}{
		"mode":     cfg.DeploymentMode,
		"features": features,
	})
}
