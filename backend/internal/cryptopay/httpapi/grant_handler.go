package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// grant_handler.go issues a short-lived payment verification grant for the
// current administrator's JWT session. The browser submits its newly entered
// authenticator code in the body. The grant service consumes the code before
// issuing a credential that the console can use on eligible routes.

// cryptoGrantSecurityVerifier issues and checks short-lived payment grants scoped to sessions.
type cryptoGrantSecurityVerifier interface {
	IssueRiskGrant(context.Context, int64, string, string) (*identity.CryptoVerificationGrant, error)
	VerifyRiskGrant(context.Context, int64, string, string) error
}

// AdminCryptoSecurityVerify issues a fixed five-minute payment capability only
// after a fresh authenticator code. A previous grant cannot mint another grant.
func (h *CryptoHandler) AdminCryptoSecurityVerify(c *gin.Context) {
	cryptoVaultHeaders(c)
	if len(c.Request.Header.Values("X-Crypto-Grant")) != 0 || len(c.Request.Header.Values("X-Crypto-TOTP")) != 0 {
		httpx.BadRequest(c, "verification requires a fresh TOTP code in the request body")
		return
	}
	var request struct {
		TotpCode string `json:"totp_code"`
	}
	if !decodeCryptoRequest(c, &request, 4<<10, "invalid verification request") {
		return
	}
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		httpx.Unauthorized(c, "authentication required")
		return
	}
	verifier, ok := h.security.(cryptoGrantSecurityVerifier)
	if !ok {
		httpx.ErrorFrom(c, identity.ErrCryptoTotpUnavailable)
		return
	}
	grant, err := verifier.IssueRiskGrant(c.Request.Context(), subject.UserID, authctx.CryptoSessionID(c), request.TotpCode)
	request.TotpCode = ""
	if httpx.ErrorFrom(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": grant})
}
