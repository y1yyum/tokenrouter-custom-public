package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/notification"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// user_bark_handler.go lets a signed-in user read and change their own Bark
// delivery settings. The request uses the authenticated account and secure
// transport. The notification service owns encryption and delivery to the
// original device before a change, while HTTP exposes only saved status.

// UserBarkHandler exposes the user's Bark push settings. The device key is
// write-only from the client's perspective: reads return only whether a key
// is configured, never the key itself.
type UserBarkHandler struct {
	service *notification.UserBarkService // user notification settings service
	config  *config.Config                // trusted proxy configuration
}

// NewUserBarkHandler uses the user notification service and host transport policy.
func NewUserBarkHandler(barkService *notification.UserBarkService, cfg *config.Config) *UserBarkHandler {
	return &UserBarkHandler{service: barkService, config: cfg}
}

// GetBarkSettings returns the configuration status.
// GET /api/v1/user/bark
func (h *UserBarkHandler) GetBarkSettings(c *gin.Context) {
	cryptoNoStore(c)
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok {
		httpx.Unauthorized(c, "User not authenticated")
		return
	}
	configured, enabled, err := h.service.Status(c.Request.Context(), subject.UserID)
	if err != nil {
		httpx.ErrorFrom(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"configured": configured,
		"enabled":    enabled,
	}})
}

// UpdateBarkSettings stores or clears the device key.
// PUT /api/v1/user/bark
func (h *UserBarkHandler) UpdateBarkSettings(c *gin.Context) {
	cryptoNoStore(c)
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok {
		httpx.Unauthorized(c, "User not authenticated")
		return
	}
	var req struct {
		DeviceKey *string `json:"device_key"`
		Enabled   *bool   `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || req.Enabled == nil {
		httpx.BadRequest(c, "Invalid request payload")
		return
	}
	if !cryptoAdminAllowHTTP() && !cryptoSecureTransport(c.Request, h.config) {
		httpx.ErrorWithDetails(c, http.StatusForbidden, "HTTPS is required to change notification settings", "BARK_HTTPS_REQUIRED", nil)
		return
	}
	// No TOTP step-up here on purpose. The Bark device key only decides where
	// THIS user's own push notifications go; it grants no balance, credential or
	// admin capability. Forcing a fresh code made the settings page unusable for
	// ordinary users, so protection is layered differently:
	//   - the key is encrypted at rest and never returned to the client;
	//   - a change is announced on the ORIGINAL device first, and the update is
	//     refused when that original notification cannot be delivered
	//     (ErrBarkOriginalNotificationFailed), so a silent redirect is impossible;
	//   - HTTPS is required to submit it.
	err := h.service.UpdateSettings(c.Request.Context(), subject.UserID, req.DeviceKey, *req.Enabled)
	if err != nil {
		if errors.Is(err, notification.ErrBarkKeyRequired) || errors.Is(err, notification.ErrBarkInvalidKey) {
			httpx.BadRequest(c, err.Error())
			return
		}
		if errors.Is(err, notification.ErrBarkOriginalNotificationFailed) {
			httpx.ErrorWithDetails(c, http.StatusServiceUnavailable, "原 Bark 设备通知失败，配置未修改，请恢复原通知通道后重试", "BARK_ORIGINAL_NOTIFICATION_FAILED", nil)
			return
		}
		httpx.ErrorFrom(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"saved": true}})
}
