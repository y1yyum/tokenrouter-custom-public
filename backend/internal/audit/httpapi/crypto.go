package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// crypto.go defines which crypto request details the host audit writer can
// retain. Wallet requests can carry recovery shards, authenticator codes, and
// credentials embedded in URLs. The audit writer omits their supplied bodies
// and query strings and takes selected operation summaries from the handlers.

const (
	// auditCtxKeyCryptoExtra carries operation summaries from crypto handlers to
	// the HTTP audit middleware on the same request.
	auditCtxKeyCryptoExtra = "audit_crypto_extra"
)

// SetCryptoAuditExtra attaches the vetted results of a crypto operation.
// Only known summary keys with the expected scalar type enter the record.
// Handlers supply operation data, and the audit entry includes the HTTP outcome.
func SetCryptoAuditExtra(c *gin.Context, values map[string]any) {
	extra := cryptoAuditExtra(c)
	for key, value := range values {
		switch key {
		case "result":
			if v, ok := value.(string); ok && (v == "success" || v == "partial" || v == "failed") {
				extra[key] = v
			}
		case "requested_count":
			if v, ok := value.(int); ok && v > 0 {
				extra[key] = v
			}
		case "hot_wallet_evm_old", "hot_wallet_evm_new", "hot_wallet_solana_old", "hot_wallet_solana_new":
			if v, ok := value.(string); ok && len(v) <= 64 {
				extra[key] = v
			}
		case "changed", "locked", "lock_created", "auto_sweep_enabled":
			if v, ok := value.(bool); ok {
				extra[key] = v
			}
		}
	}
	c.Set(auditCtxKeyCryptoExtra, extra)
}

// cryptoAuditExtra returns the audit record's initial operation summary.
// A request without any handler summary starts with an empty map that the
// middleware can populate with general audit metadata.
func cryptoAuditExtra(c *gin.Context) map[string]any {
	extra, _ := c.Get(auditCtxKeyCryptoExtra)
	if values, ok := extra.(map[string]any); ok {
		return values
	}
	return make(map[string]any)
}

// cryptoAuditRoute identifies routes whose query strings may contain credentials.
func cryptoAuditRoute(path string) bool {
	return strings.HasPrefix(path, "/api/v1/admin/crypto/") || path == "/api/v1/user/bark"
}

// cryptoAuditOmitBody prevents credential-bearing requests from entering body capture.
func cryptoAuditOmitBody(c *gin.Context) bool {
	return c.Request.Method != http.MethodGet && cryptoAuditRoute(c.FullPath())
}
