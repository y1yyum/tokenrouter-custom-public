package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/sweeper"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// mnemonics.go exports the two seed phrases for a verified payment operator.
// The wallet service performs any recovery-shard check and durably records the
// successful export before the phrases can be returned. HTTP responses carrying
// vault material use the no-store headers.

// cryptoMnemonicExporter reads the wallet seeds with the active wallet authorization.
type cryptoMnemonicExporter interface {
	ExportMnemonics(ctx context.Context) (*sweeper.MnemonicExport, error)
}

// CryptoMnemonicExportRequest is accepted once per explicit user action. A
// data-key vault additionally requires one operator-held recovery share.
type CryptoMnemonicExportRequest struct {
	Share string `json:"share"`
}

// cryptoMnemonicShareExporter is implemented by the wallet facade for vaults
// sealed with a data key.
type cryptoMnemonicShareExporter interface {
	ExportMnemonicsWithShare(context.Context, string) (*sweeper.MnemonicExport, error)
}

// CryptoMnemonicExportResponse contains exactly the two independent seed fields.
type CryptoMnemonicExportResponse struct {
	DepositMnemonic string `json:"deposit_mnemonic"`
	HotMnemonic     string `json:"hot_mnemonic"`
}

// AdminExportMnemonics returns plaintext only after the wallet has durably
// audited the successful decryption after fresh backend 2FA.
func (h *CryptoHandler) AdminExportMnemonics(c *gin.Context) {
	cryptoVaultHeaders(c)
	var req CryptoMnemonicExportRequest
	if !decodeCryptoMnemonicRequest(c, &req) {
		return
	}
	if h.mnemonicExporter == nil {
		httpx.Error(c, http.StatusServiceUnavailable, "Mnemonic export unavailable")
		return
	}
	defer func() { req.Share = "" }()
	var result *sweeper.MnemonicExport
	var err error
	if strings.TrimSpace(req.Share) != "" {
		shareExporter, ok := h.mnemonicExporter.(cryptoMnemonicShareExporter)
		if !ok {
			httpx.Error(c, http.StatusConflict, "recovery shard is required to export mnemonics")
			return
		}
		result, err = shareExporter.ExportMnemonicsWithShare(cryptoAuditContext(c), req.Share)
	} else {
		result, err = h.mnemonicExporter.ExportMnemonics(cryptoAuditContext(c))
	}
	if writeCryptoSignerError(c, err) {
		return
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		message := "Mnemonic export unavailable"

		httpx.Error(c, status, message)
		return
	}
	if result == nil || strings.TrimSpace(result.DepositMnemonic) == "" || strings.TrimSpace(result.HotMnemonic) == "" {
		httpx.Error(c, http.StatusServiceUnavailable, "Mnemonic export unavailable")
		return
	}
	defer func() { result.DepositMnemonic, result.HotMnemonic = "", "" }()
	c.JSON(http.StatusOK, CryptoMnemonicExportResponse{
		DepositMnemonic: result.DepositMnemonic,
		HotMnemonic:     result.HotMnemonic,
	})
}

// decodeCryptoMnemonicRequest parses one strict JSON value within the vault limit.
// The byte buffer is cleared before return, and invalid input writes the client error.
func decodeCryptoMnemonicRequest(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		httpx.BadRequest(c, "Invalid mnemonic export request")
		return false
	}
	defer func() { clear(raw) }()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		httpx.BadRequest(c, "Invalid mnemonic export request")
		return false
	}
	return true
}
