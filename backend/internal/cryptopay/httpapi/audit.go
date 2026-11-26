package httpapi

import (
	"context"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/wallet"
	identityhttp "github.com/TokenFlux/TokenRouter/internal/identity/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
)

// audit.go attaches request provenance to wallet security events. The wallet
// audit writer receives the same authenticated actor as the host HTTP audit,
// together with request metadata that lets an operator correlate both entries.

// cryptoAuditContext attaches the authenticated admin identity and request
// provenance to the context passed to the wallet audit writer. The caller
// passes through CryptoAdminSecurity before attaching this metadata.
func cryptoAuditContext(c *gin.Context) context.Context {
	requestContext := c.Request.Context()
	actor := wallet.AuditActor{
		Email:     c.GetString(authctx.ContextKeyAuthEmail),
		ClientIP:  identityhttp.SecurityClientIP(c),
		UserAgent: c.Request.UserAgent(),
		RequestID: cryptoRequestID(requestContext),
	}
	if principal, ok := authctx.GetPrincipal(c); ok && principal.UserID > 0 {
		userID := principal.UserID
		actor.UserID = &userID
		actor.AuthMethod = principal.CredentialKind
	}
	return wallet.WithAuditActor(requestContext, actor)
}

// cryptoRequestID returns the identifier published by the HTTP request middleware.
func cryptoRequestID(ctx context.Context) string {
	requestID, _ := ctx.Value(telemetry.RequestID).(string)
	return requestID
}
