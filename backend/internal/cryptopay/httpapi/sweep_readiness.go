package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// sweep_readiness.go translates the wallet facade's current sweep readiness
// into an administrator response. The console displays that result directly so
// individual wallet or settings views cannot suggest a sweep is ready when the
// service would refuse to start it.

// cryptoSweepGateReader is the backend-side source of truth for the "can I run
// a sweep?" question. The console renders its reason code verbatim instead of
// combining wallet/status, config and signer/status on its own.
type cryptoSweepGateReader interface {
	SweepGate(context.Context) (*cryptopay.SweepGate, error)
}

// sweepGateReader selects the wallet readiness reader when one is available.
func (h *CryptoHandler) sweepGateReader() (cryptoSweepGateReader, bool) {
	if h.sweepGate != nil {
		return h.sweepGate, true
	}
	reader, ok := any(h.service).(cryptoSweepGateReader)
	return reader, ok && h.service != nil
}

// AdminSweepReadiness reports what a real sweep still needs. It is a read-only
// GET: the destination is only reported as configured/pinned, never echoed.
func (h *CryptoHandler) AdminSweepReadiness(c *gin.Context) {
	reader, ok := h.sweepGateReader()
	if !ok {
		httpx.Error(c, http.StatusServiceUnavailable, "crypto sweep unavailable")
		return
	}
	gate, err := reader.SweepGate(cryptoAuditContext(c))
	if httpx.ErrorFrom(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gate})
}
