package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/wallet"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// private_key.go exports the private key identified by an existing deposit
// assignment. The wallet uses its own address and derivation data, including a
// recovery shard when applicable. The route provides a cache-resistant response
// and maps credential and wallet failures to fixed messages.

// cryptoAddressPrivateKeyExporter derives the key for the specified deposit assignment.
type cryptoAddressPrivateKeyExporter interface {
	ExportAddressPrivateKey(context.Context, int64) (*wallet.AddressPrivateKeyExport, error)
}

// cryptoAddressPrivateKeyShareExporter authorizes one key export with an operator shard.
type cryptoAddressPrivateKeyShareExporter interface {
	ExportAddressPrivateKeyWithShare(context.Context, int64, string) (*wallet.AddressPrivateKeyExport, error)
}

// AdminExportAddressPrivateKey runs inside the same operator JWT, transport,
// payment verification and security-notification boundary as mnemonic export.
// Only a database assignment ID is accepted; no caller-controlled derivation.
func (h *CryptoHandler) AdminExportAddressPrivateKey(c *gin.Context) {
	cryptoVaultHeaders(c)
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.BadRequest(c, "Invalid address ID")
		return
	}
	var req struct {
		Share string `json:"share"`
	}
	if !decodeCryptoRequest(c, &req, 4<<10, "Invalid private key export request") {
		return
	}
	defer func() { req.Share = "" }()
	if h.privateKeyExporter == nil {
		httpx.Error(c, http.StatusServiceUnavailable, "Private key export unavailable")
		return
	}
	var result *wallet.AddressPrivateKeyExport
	if strings.TrimSpace(req.Share) != "" {
		shareExporter, ok := h.privateKeyExporter.(cryptoAddressPrivateKeyShareExporter)
		if !ok {
			httpx.Error(c, http.StatusConflict, "recovery shard is required to export a private key")
			return
		}
		result, err = shareExporter.ExportAddressPrivateKeyWithShare(cryptoAuditContext(c), id, req.Share)
	} else {
		result, err = h.privateKeyExporter.ExportAddressPrivateKey(cryptoAuditContext(c), id)
	}
	if result != nil {
		defer func() { result.PrivateKey = "" }()
	}
	if writeCryptoSignerError(c, err) {
		return
	}
	if err != nil {
		if errors.Is(err, wallet.ErrExportAddressNotFound) {
			httpx.Error(c, http.StatusNotFound, "Deposit address not found")
			return
		}
		// Vault/driver errors can contain sensitive data. Never log or echo them.
		httpx.Error(c, http.StatusServiceUnavailable, "Private key export unavailable")
		return
	}
	if result == nil || result.AddressID != id || strings.TrimSpace(result.PrivateKey) == "" ||
		strings.TrimSpace(result.Address) == "" {
		httpx.Error(c, http.StatusServiceUnavailable, "Private key export unavailable")
		return
	}
	c.JSON(http.StatusOK, result)
}
