package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// deposit_handler.go serves current deposit instructions, network status, and
// the caller's deposit history. Each request uses the host JWT subject for its
// user scope. The crypto service owns address allocation and supplies the view
// of configured networks and assets.

// CryptoDepositHandler serves the standalone deposit page: per-chain
// addresses, the credit rate and the user's own deposit history.
type CryptoDepositHandler struct {
	service *cryptopay.Service
}

// NewCryptoDepositHandler uses the running crypto service for customer data.
func NewCryptoDepositHandler(service *cryptopay.Service) *CryptoDepositHandler {
	return &CryptoDepositHandler{service: service}
}

// GetDepositInfo returns the addresses, chains, rate and limits the page
// needs in one call.
// GET /api/v1/crypto/deposit-info
func (h *CryptoDepositHandler) GetDepositInfo(c *gin.Context) {
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok {
		httpx.Unauthorized(c, "User not authenticated")
		return
	}
	info, err := h.service.DepositInfo(c.Request.Context(), subject.UserID)
	if err != nil {
		httpx.ErrorFrom(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": info})
}

// GetDepositNetworkStatus reports only customer-safe network status.
// GET /api/v1/crypto/deposit-network-status
func (h *CryptoDepositHandler) GetDepositNetworkStatus(c *gin.Context) {
	cryptoNoStore(c)
	if _, ok := authctx.GetAuthSubjectFromContext(c); !ok {
		httpx.Unauthorized(c, "User not authenticated")
		return
	}
	httpx.Success(c, h.service.DepositNetworkStatus(c.Request.Context()))
}

// GetDepositHistory lists the caller's own deposits.
// GET /api/v1/crypto/deposit-history
func (h *CryptoDepositHandler) GetDepositHistory(c *gin.Context) {
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok {
		httpx.Unauthorized(c, "User not authenticated")
		return
	}
	records, err := h.service.UserDepositHistory(c.Request.Context(), subject.UserID, 50)
	if err != nil {
		httpx.ErrorFrom(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": records})
}
