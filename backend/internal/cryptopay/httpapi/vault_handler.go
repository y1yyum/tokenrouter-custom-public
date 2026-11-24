package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// vault_handler.go applies no-store and strict request-decoding rules to the
// vault HTTP endpoints. It also accepts a hot wallet mnemonic for the wallet
// service to import. Domain failures are mapped to fixed messages so callers
// can resolve wallet state without exposing submitted credentials.

// cryptoVaultHeaders prevents caching and MIME sniffing of vault responses.
func cryptoVaultHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	c.Header("X-Content-Type-Options", "nosniff")
}

// AdminImportHotMnemonic replaces the hot wallet seed after payment verification.
// The wallet service applies its security checks and audit before reporting that
// the import succeeded.
func (h *CryptoHandler) AdminImportHotMnemonic(c *gin.Context) {
	cryptoVaultHeaders(c)
	var req struct {
		HotMnemonic string `json:"hot_mnemonic"`
		Share       string `json:"share"`
	}
	if !decodeCryptoVaultRequest(c, &req) {
		return
	}
	if strings.TrimSpace(req.HotMnemonic) == "" || len(req.HotMnemonic) > 1024 {
		httpx.BadRequest(c, "A valid hot-wallet mnemonic is required")
		return
	}
	if h.service == nil {
		httpx.Error(c, http.StatusServiceUnavailable, "Wallet unavailable")
		return
	}
	err := h.service.ImportHotMnemonic(cryptoAuditContext(c), req.HotMnemonic)
	req.HotMnemonic = ""
	if err != nil {
		httpx.Error(c, http.StatusConflict, "Hot-wallet import refused; check the wallet configuration and unresolved sweeps")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"saved": true}})
}

// decodeCryptoVaultRequest parses strict JSON within the four KiB vault limit.
func decodeCryptoVaultRequest(c *gin.Context, target any) bool {
	return decodeCryptoRequest(c, target, 4<<10, "Invalid vault request")
}

// decodeCryptoRequest reads one bounded JSON value into the supplied target.
// Unknown fields and trailing JSON write a bad request so validation ends before
// any payment action is dispatched.
func decodeCryptoRequest(c *gin.Context, target any, limit int64, message string) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		httpx.BadRequest(c, message)
		return false
	}
	return true
}
