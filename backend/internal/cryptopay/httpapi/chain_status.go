package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// chain_status.go exposes operational scan progress and node health to payment
// administrators. The chain status service returns host labels and counters for
// display. The handler returns a fixed failure message because RPC errors can
// include configured endpoint URLs and provider credentials.

// AdminChainStatus reports every chain's scan progress and RPC node health.
// Read-only: it never writes the database, changes configuration, starts a scan
// or moves a cursor. Node URLs, paths and keys never appear in the response;
// nodes are identified by host and a short label only.
// GET /api/v1/admin/crypto/chains/status
func (h *CryptoHandler) AdminChainStatus(c *gin.Context) {
	cryptoNoStore(c)
	if h.service == nil {
		httpx.Error(c, http.StatusServiceUnavailable, "Chain status unavailable")
		return
	}
	report, err := h.service.ChainStatus(c.Request.Context())
	if err != nil {
		// The cause is deliberately not logged or returned: probe and driver
		// errors can carry endpoint URLs.
		httpx.Error(c, http.StatusServiceUnavailable, "Chain status unavailable")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": report})
}
