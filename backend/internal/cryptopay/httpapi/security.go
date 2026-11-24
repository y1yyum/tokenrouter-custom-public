package httpapi

import (
	"context"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// security.go applies payment-specific administration and transport policy
// after the host authenticates the caller. CryptoAdminSecurity requires an
// operator from the deployment allowlist and ties payment verification to the
// principal's JWT session. Some operations consume a new code in the body,
// while eligible operations can use a five-minute session grant.
//
// The HTTPS check uses an actual TLS connection or a forwarded HTTPS scheme
// from an explicitly trusted proxy. A direct local tunnel may use HTTP only
// when the socket peer and Host are loopback and the request has no forwarding
// headers. Read-only review installations keep an independent route allowlist.

// cryptoAdminSecurityVerifier checks operator access and manages payment authenticator state.
type cryptoAdminSecurityVerifier interface {
	CurrentOperator(context.Context, int64) (*identity.User, error)
	VerifyRisk(context.Context, int64, string) error
	Status(context.Context, int64) (*identity.CryptoSecurityStatus, error)
	InitiateSetup(context.Context, int64, string) (*identity.TotpSetupResponse, error)
	Enable(context.Context, int64, string, string, string) error
}

// CryptoAdminSecurity applies the local instance, operator, and transport
// checks to every administrator crypto request. Mutations and sensitive reads
// require payment verification by default. A route that validates the body code
// or decides whether a submitted patch needs verification retains that check
// in its handler.
func (h *CryptoHandler) CryptoAdminSecurity(c *gin.Context) {
	// Admin responses may contain RPC credentials, wallet addresses, or payment
	// history. Never let browsers or intermediary caches persist them.
	cryptoNoStore(c)
	if !mode.InstanceEnabled() {
		httpx.ErrorWithDetails(c, 503, "crypto is disabled on this instance", "CRYPTO_DISABLED", nil)
		c.Abort()
		return
	}
	_, ok := h.cryptoJWTAdmin(c)
	if !ok {
		return
	}
	path := c.FullPath()
	setup := c.Request.Method == http.MethodPost && (strings.HasSuffix(path, "/security/setup") || strings.HasSuffix(path, "/security/enable"))
	verify := c.Request.Method == http.MethodPost && strings.HasSuffix(path, "/security/verify")
	intent := c.Request.Method == http.MethodPost && strings.HasSuffix(path, "/security/operation-intent")
	configPatch := c.Request.Method == http.MethodPut && strings.HasSuffix(path, "/crypto/config")
	// Shard setup/re-split/auto-sweep, the pinned-destination override and the
	// bounded signer unlock verify a freshly typed authenticator code in the
	// request body; a five-minute grant is not accepted for these.
	signerFresh := c.Request.Method == http.MethodPost &&
		(strings.HasSuffix(path, "/signer/setup") || strings.HasSuffix(path, "/signer/resplit") ||
			strings.HasSuffix(path, "/signer/auto-sweep") || strings.HasSuffix(path, "/signer/sweep-destination") ||
			strings.HasSuffix(path, "/signer/unlock"))
	// A cursor-forward scan is idempotent and uses the normal receipt,
	// confirmation and ledger path. Keep administrator JWT, HTTPS, instance and
	// read-only guards, but do not require a second factor just to retry it.
	manualScan := c.Request.Method == http.MethodPost && strings.HasSuffix(path, "/crypto/scan/run")
	risky := !manualScan && (c.Request.Method != http.MethodGet || strings.HasSuffix(path, "/deposits.csv") ||
		strings.HasSuffix(path, "/config/private"))
	if mode.ReadOnly() && !cryptoTestModeRouteAllowed(c.Request.Method, path) {
		httpx.ErrorWithDetails(c, http.StatusForbidden, "crypto test instance is read-only", "CRYPTO_TEST_MODE_READ_ONLY", nil)
		c.Abort()
		return
	}
	if !cryptoAdminAllowHTTP() && !cryptoSecureTransport(c.Request, h.securityConfig) {
		httpx.ErrorWithDetails(c, 403, "HTTPS or a loopback tunnel is required", "CRYPTO_HTTPS_REQUIRED", nil)
		c.Abort()
		return
	}
	if risky && !setup && !verify && !intent && !configPatch && !signerFresh {
		if !h.requireCryptoRisk(c) {
			return
		}
		if action := cryptoVerifiedOperation(c); action != "" && !h.notifyWalletAction(c, action, "verified") {
			c.Abort()
			return
		}
	}
	c.Next()
}

// requireCryptoRisk verifies a session-bound grant or a fresh backend code. PUT /config calls this
// after inspecting the actual patch; other sensitive routes call it in their
// group middleware. A missing verifier always fails closed.
func (h *CryptoHandler) requireCryptoRisk(c *gin.Context) bool {
	if h.security == nil {
		httpx.ErrorFrom(c, identity.ErrCryptoTotpUnavailable)
		c.Abort()
		return false
	}
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		httpx.ErrorFrom(c, identity.ErrCryptoAdminRequired)
		c.Abort()
		return false
	}
	codes := c.Request.Header.Values("X-Crypto-TOTP")
	grants := c.Request.Header.Values("X-Crypto-Grant")
	if len(grants) != 0 {
		if len(grants) != 1 || len(codes) != 0 {
			httpx.ErrorFrom(c, identity.ErrCryptoGrantRequired)
			c.Abort()
			return false
		}
		verifier, ok := h.security.(cryptoGrantSecurityVerifier)
		if !ok {
			httpx.ErrorFrom(c, identity.ErrCryptoTotpUnavailable)
			c.Abort()
			return false
		}
		if httpx.ErrorFrom(c, verifier.VerifyRiskGrant(c.Request.Context(), subject.UserID, authctx.CryptoSessionID(c), grants[0])) {
			c.Abort()
			return false
		}
		return true
	}
	code := ""
	if len(codes) == 1 {
		code = codes[0]
	}
	if httpx.ErrorFrom(c, h.security.VerifyRisk(c.Request.Context(), subject.UserID, code)) {
		c.Abort()
		return false
	}
	return true
}

// CryptoUserAccess checks whether this host can serve customer deposits.
// A read-only review instance rejects the request before address allocation.
// The persistent key is required even when another host owns scanning.
func (h *CryptoHandler) CryptoUserAccess(c *gin.Context) {
	cryptoNoStore(c)
	if mode.ReadOnly() {
		httpx.ErrorWithDetails(c, http.StatusForbidden, "customer crypto routes are disabled on this test instance", "CRYPTO_TEST_MODE_READ_ONLY", nil)
		c.Abort()
		return
	}
	cfg := h.securityConfig
	if cfg == nil || !mode.UserAPIReady(cfg.Security.SecretEncryptionKey, cfg.Security.SecretEncryptionKeyConfigured) {
		httpx.ErrorWithDetails(c, 503, "crypto is disabled on this instance", "CRYPTO_DISABLED", nil)
		c.Abort()
		return
	}
	c.Next()
}

// cryptoJWTAdmin admits only an operator authenticated by the host Bearer middleware.
// Any additional API key or alternate WebSocket transport ends the request before
// wallet authorization can be evaluated.
func (h *CryptoHandler) cryptoJWTAdmin(c *gin.Context) (int64, bool) {
	// Validate the same Bearer transport used by the panel identity middleware.
	// An upgrade can select a different token before the Authorization header.
	principal, authenticated := authctx.GetPrincipal(c)
	if !authenticated || authctx.CryptoSessionID(c) == "" {
		httpx.ErrorWithDetails(c, 403, "an administrator JWT session is required", "CRYPTO_ADMIN_REQUIRED", nil)
		c.Abort()
		return 0, false
	}
	if h.security == nil {
		httpx.ErrorFrom(c, identity.ErrCryptoTotpUnavailable)
		c.Abort()
		return 0, false
	}
	_, err := h.security.CurrentOperator(c.Request.Context(), principal.UserID)
	if httpx.ErrorFrom(c, err) {
		c.Abort()
		return 0, false
	}
	return principal.UserID, true
}

// cryptoNoStore prevents storage of the current payment response.
func cryptoNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
}

// cryptoSecureTransport accepts TLS, HTTPS forwarded by an explicitly configured
// proxy, or a direct loopback tunnel. The loopback branch checks both socket
// and Host and rejects forwarding headers before allowing the request.
func cryptoSecureTransport(r *http.Request, cfg *config.Config) bool {
	if r.TLS != nil {
		return true
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	peerIP := net.ParseIP(peer)
	if peerIP == nil {
		return false
	}
	// Forwarded schemes are accepted only from explicitly configured proxies.
	if cfg != nil && cfg.Server.ClientIP.TrustedProxiesConfigured && len(r.Header.Values("X-Forwarded-Proto")) == 1 &&
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		for _, entry := range cfg.Server.ClientIP.TrustedProxies {
			if ip := net.ParseIP(entry); ip != nil && ip.Equal(peerIP) {
				return true
			}
			if _, network, err := net.ParseCIDR(entry); err == nil {
				bits, _ := network.Mask.Size()
				// A trust-all configuration cannot authorize a public HTTPS claim.
				if bits > 0 && network.Contains(peerIP) {
					return true
				}
			}
		}
	}
	// Loopback means both the actual socket peer and Host. Forwarded requests
	// cannot disguise a public HTTP request as a localhost SSH tunnel.
	if !peerIP.IsLoopback() {
		return false
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-IP"} {
		if len(r.Header.Values(header)) > 0 {
			return false
		}
	}
	host := r.Host
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

// AdminCryptoSecurityStatus reports effective payment-TOTP readiness.
func (h *CryptoHandler) AdminCryptoSecurityStatus(c *gin.Context) {
	cryptoNoStore(c)
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok {
		httpx.Unauthorized(c, "authentication required")
		return
	}
	status, err := h.security.Status(c.Request.Context(), subject.UserID)
	if httpx.ErrorFrom(c, err) {
		return
	}
	httpx.Success(c, status)
}

// AdminCryptoSecuritySetup returns a short-lived seed bound to the current JWT session.
func (h *CryptoHandler) AdminCryptoSecuritySetup(c *gin.Context) {
	cryptoNoStore(c)
	var req struct{}
	if !decodeCryptoRequest(c, &req, 4<<10, "invalid setup request") {
		return
	}
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok {
		httpx.Unauthorized(c, "authentication required")
		return
	}
	result, err := h.security.InitiateSetup(c.Request.Context(), subject.UserID, authctx.CryptoSessionID(c))
	if httpx.ErrorFrom(c, err) {
		return
	}
	httpx.Success(c, result)
}

// AdminCryptoSecurityEnable never trusts a client enabled flag.
func (h *CryptoHandler) AdminCryptoSecurityEnable(c *gin.Context) {
	cryptoNoStore(c)
	var req struct {
		TotpCode   string `json:"totp_code"`
		SetupToken string `json:"setup_token"`
	}
	if !decodeCryptoRequest(c, &req, 4<<10, "invalid enable request") {
		return
	}
	subject, ok := authctx.GetAuthSubjectFromContext(c)
	if !ok {
		httpx.Unauthorized(c, "authentication required")
		return
	}
	if httpx.ErrorFrom(c, h.security.Enable(c.Request.Context(), subject.UserID, authctx.CryptoSessionID(c), req.TotpCode, req.SetupToken)) {
		return
	}
	httpx.Success(c, gin.H{"success": true})
}

// cryptoAdminAllowHTTP reports the process-local opt-in for crypto HTTP.
// Missing or malformed values keep the normal transport requirement in effect.
// Administrator identity and every operation-specific check still run.
func cryptoAdminAllowHTTP() bool {
	allowed, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("CRYPTO_ADMIN_ALLOW_HTTP")))
	return err == nil && allowed
}
