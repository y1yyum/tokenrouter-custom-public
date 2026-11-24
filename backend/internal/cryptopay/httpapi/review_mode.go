package httpapi

import (
	"net/http"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
	"github.com/gin-gonic/gin"
)

// review_mode.go filters HTTP requests on a local read-only review instance.
// The allowlist keeps health, login, and selected console reads available.
// It is evaluated independently of shared site settings, which may belong to
// the production installation using the same database.

// CryptoAdminTestModeGuard limits a shared-production-database review instance
// to authentication and crypto administration while it is read-only. Explicit
// write opt-in restores normal routes and their existing authentication; startup
// isolation remains enabled. Mount this guard before serving panel, gateway,
// or static routes so the read-only allowlist covers the entire installation.
func CryptoAdminTestModeGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !mode.ReadOnly() || cryptoTestModeRouteAllowed(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}
		httpx.ErrorWithDetails(c, http.StatusForbidden, "test instance route is read-only", "CRYPTO_TEST_MODE_READ_ONLY", nil)
		c.Abort()
	}
}

// cryptoTestModeRouteAllowed admits only the endpoints needed to inspect crypto.
// Unknown API paths and all non-bootstrap mutations stay blocked on a
// read-only review instance, including paths that the router does not mount.
func cryptoTestModeRouteAllowed(method, path string) bool {
	if method == http.MethodOptions {
		return true
	}
	if (method == http.MethodGet || method == http.MethodHead) &&
		(path == "/health" || path == "/readyz" || path == "/setup/status") {
		return true
	}
	if method == http.MethodPost && (path == "/api/v1/auth/login" || path == "/api/v1/auth/login/2fa") {
		return true
	}
	if method == http.MethodGet && (path == "/api/v1/auth/me" || path == "/api/v1/settings/public" ||
		path == "/api/v1/admin/crypto/security/status") {
		return true
	}
	if method == http.MethodGet {
		switch path {
		case "/api/v1/admin/crypto/config", "/api/v1/admin/crypto/addresses",
			"/api/v1/admin/crypto/sweep/plan", "/api/v1/admin/crypto/sweep/records",
			"/api/v1/admin/crypto/address-owners",
			"/api/v1/admin/crypto/deposits", "/api/v1/admin/crypto/wallet/status",
			"/api/v1/admin/crypto/pool", "/api/v1/admin/crypto/hot-wallet",
			"/api/v1/admin/crypto/hot-wallet/address", "/api/v1/admin/crypto/rpc/candidates":
			return true
		}
	}
	if strings.HasPrefix(path, "/api/") || cryptoTestModeGatewayPath(path) {
		return false
	}
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	// Permit direct loads of the host's console subpages. Their API calls
	// still pass through the separate read-only allowlist above.
	switch path {
	case "/", "/login", "/admin/login", "/admin/crypto", "/admin/crypto/addresses",
		"/admin/crypto/chains", "/admin/crypto/deposits", "/admin/crypto/settings",
		"/admin/crypto/sweeps", "/site-logo", "/favicon.ico", "/robots.txt":
		return true
	default:
		return strings.HasPrefix(path, "/assets/")
	}
}

// cryptoTestModeGatewayPath identifies compatibility paths outside the API group.
func cryptoTestModeGatewayPath(path string) bool {
	for _, prefix := range []string{
		"/v1/", "/v1beta/", "/responses", "/chat/completions", "/messages", "/models",
		"/videos", "/images", "/embeddings", "/completions", "/moderations", "/audio/", "/realtime/",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
