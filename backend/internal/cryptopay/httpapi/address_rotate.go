package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// address_rotate.go retains the address rotation endpoint with the fixed
// source's explicit refusal. Retired assignments need compatible wallet integrity
// checks and deposit attribution before rotation can be enabled. The current
// handler returns a conflict and leaves the assignment untouched.

// AdminRotateAddress keeps the administrative route but prevents rotation until
// the wallet integrity guard and deposit attribution both support retired addresses.
func (h *CryptoHandler) AdminRotateAddress(c *gin.Context) {
	cryptoVaultHeaders(c)
	httpx.ErrorWithDetails(c, http.StatusConflict, "Deposit address rotation is temporarily unavailable", "DEPOSIT_ADDRESS_ROTATION_UNAVAILABLE", nil)
}
