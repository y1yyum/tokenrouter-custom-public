package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// security_guard_handler.go provides the wallet lock status and an explicit
// recovery endpoint. Recovery runs the wallet integrity checks under the
// payment administration guard. The deposit-channel switch is a separate
// settings edit after the operator verifies recovery.

// AdminWalletSecurity reports lock metadata under the payment operator's
// authenticated session and transport guard. The wallet facade returns its
// security status without vault credentials.
func (h *CryptoHandler) AdminWalletSecurity(c *gin.Context) {
	cryptoNoStore(c)
	if h.service == nil {
		httpx.Error(c, http.StatusServiceUnavailable, "Wallet unavailable")
		return
	}
	status, err := h.service.WalletSecurity(c.Request.Context())
	if err != nil {
		httpx.Error(c, http.StatusServiceUnavailable, "Wallet security status unavailable")
		return
	}
	httpx.Success(c, status)
}

// AdminRecoverWalletSecurity clears the lock only after the wallet integrity
// checks pass. The route requires payment verification and an explicit
// confirmation. The operator enables the deposit channel in a separate action.
func (h *CryptoHandler) AdminRecoverWalletSecurity(c *gin.Context) {
	cryptoNoStore(c)
	var req struct {
		Confirmation string `json:"confirmation"`
	}
	if !decodeCryptoRequest(c, &req, 4<<10, "Invalid recovery request") {
		return
	}
	if req.Confirmation != "RECOVER" {
		httpx.BadRequest(c, "Explicit recovery confirmation is required")
		return
	}
	if h.service == nil {
		httpx.Error(c, http.StatusServiceUnavailable, "Wallet unavailable")
		return
	}
	status, err := h.service.RecoverWalletSecurity(cryptoAuditContext(c))
	if err != nil {
		httpx.Error(c, http.StatusConflict, "Recovery refused; repair wallet address integrity before retrying")
		return
	}
	httpx.Success(c, status)
}
