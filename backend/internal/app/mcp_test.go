package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	keyhttp "github.com/TokenFlux/TokenRouter/internal/apikey/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	identityprovider "github.com/TokenFlux/TokenRouter/internal/identity/provider"
	"github.com/TokenFlux/TokenRouter/internal/infra/crypto"
	mcphttp "github.com/TokenFlux/TokenRouter/internal/mcp/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/server/adminapi"
	"github.com/TokenFlux/TokenRouter/internal/server/middleware"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// mcp_test.go verifies browser-origin admission and durable MCP artifact encryption.
// The tests check that backend restarts can decrypt surviving files and configured
// TOTP encryption keeps its existing key. Browser admission and OAuth discovery
// use the same configured frontend origin. The route assembly test checks
// that the operator console can start without adding delegable wallet actions.

// TestMCPArtifactCipherUsesDurableOAuthKey recreates encryption after a backend restart.
// A different temporary TOTP key must not prevent reading files encrypted with
// the persisted OAuth key, while a configured TOTP cipher retains its identity.
func TestMCPArtifactCipherUsesDurableOAuthKey(t *testing.T) {
	first, err := crypto.NewAESEncryptor(bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	second, err := crypto.NewAESEncryptor(bytes.Repeat([]byte{2}, 32))
	require.NoError(t, err)
	cfg := &config.Config{MCPOAuthSigningSecret: strings.Repeat("ab", 32)}
	a, err := mcpArtifactCipher(first, cfg)
	require.NoError(t, err)
	b, err := mcpArtifactCipher(second, cfg)
	require.NoError(t, err)
	ciphertext, err := a.Encrypt("protected fixture")
	require.NoError(t, err)
	plaintext, err := b.Decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, "protected fixture", plaintext)
	_, err = first.Decrypt(ciphertext)
	require.Error(t, err)
	cfg.Security.SecretEncryptionKeyConfigured = true
	original, err := mcpArtifactCipher(first, cfg)
	require.NoError(t, err)
	require.Same(t, first, original)
	cfg.Security.SecretEncryptionKeyConfigured = false
	cfg.MCPOAuthSigningSecret = ""
	_, err = mcpArtifactCipher(first, cfg)
	require.Error(t, err)
}

// TestMCPBrowserOriginUsesFrontendURL assembles the transport with a deployment site address.
// The configured browser must reach credential authentication, and the challenge
// must advertise discovery at that same public origin.
func TestMCPBrowserOriginUsesFrontendURL(t *testing.T) {
	repo := &mcpSettingsFixture{values: map[string]string{}}
	store := settings.New(repo)
	cfg := &config.Config{MCPOAuthSigningSecret: strings.Repeat("ab", 32)}
	cfg.Site.FrontendURL = "https://frontend.example"
	oauth := identityprovider.NewMCPOAuthServer(nil, nil, &mcpOAuthFrontend{store: store, frontendURL: cfg.Site.FrontendURL}, bytes.Repeat([]byte{4}, 32))
	handler, err := provideMCPHTTP(oauth, nil, nil, nil, nil, nil, nil, store, cfg)
	require.NoError(t, err)
	router := gin.New()
	require.NoError(t, handler.Mount(router, func(c *gin.Context) { c.Next() }, func(c *gin.Context) { c.Next() }))
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Origin", cfg.Site.FrontendURL)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Contains(t, response.Header().Get("WWW-Authenticate"), "https://frontend.example/.well-known/oauth-protected-resource/mcp")
}

// TestCryptoAdminRoutesAreDirectOnly mounts the application's real crypto and
// MCP registrars in startup order. All wallet routes must remain reachable by
// the router, reject machine keys, and stay outside delegation.
func TestCryptoAdminRoutesAreDirectOnly(t *testing.T) {
	t.Setenv("CRYPTO_INSTANCE_ENABLED", "true")
	const cryptoPrefix = "/api/v1/admin/crypto/"
	router := gin.New()
	admin := func(c *gin.Context) {
		principal := identity.Principal{UserID: 1, Role: "admin", CredentialKind: "jwt", SessionID: "test-session"}
		if c.GetHeader("X-API-Key") != "" {
			principal.CredentialKind = "admin_api_key"
			principal.SessionID = ""
		}
		authctx.SetPrincipal(c, principal, 0, "")
		c.Next()
	}
	security := httpRouteSecurity{JWT: inventoryJWT, Admin: admin, Audit: inventoryAudit, BackendUser: inventoryBackendUser, Panel: middleware.NewPanelRateLimiter(nil, nil)}
	rotation := keyhttp.NewKeyRotationGateHandler(nil)
	provideCryptoRouteMount(nil, nil, nil, nil, &config.Config{}, nil, rotation)(router.Group("/api/v1"), security)
	store := settings.New(&mcpSettingsFixture{values: map[string]string{}})
	frontend := &mcpOAuthFrontend{store: store, frontendURL: "https://frontend.example"}
	oauth := identityprovider.NewMCPOAuthServer(nil, nil, frontend, bytes.Repeat([]byte{4}, 32))
	handler := mcphttp.NewHandler(nil, nil, nil, nil, nil, mcphttp.Options{OAuth: oauth})
	require.NoError(t, handler.Mount(router, admin, inventoryAudit))

	var cryptoRoutes []gin.RouteInfo
	for _, route := range router.Routes() {
		if strings.HasPrefix(route.Path, cryptoPrefix) {
			cryptoRoutes = append(cryptoRoutes, route)
		}
	}
	require.Len(t, cryptoRoutes, 49)
	for _, route := range cryptoRoutes {
		path := strings.ReplaceAll(route.Path, ":id", "1")
		request := httptest.NewRequest(route.Method, path, nil)
		request.Header.Set("X-API-Key", "fixture-key")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, http.StatusForbidden, response.Code, "%s %s", route.Method, route.Path)
		require.Contains(t, response.Body.String(), `"reason":"CRYPTO_ADMIN_REQUIRED"`, "%s %s", route.Method, route.Path)
	}

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/mcp/operations", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var result struct {
		Data []adminapi.Operation `json:"data"` // published permission entries
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	require.True(t, slices.ContainsFunc(result.Data, func(op adminapi.Operation) bool { return op.ID == "mcp.tokens.list" }))
	for _, operation := range result.Data {
		require.False(t, strings.HasPrefix(operation.Path, cryptoPrefix), operation.ID)
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/admin/mcp/openapi", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.NotContains(t, response.Body.String(), cryptoPrefix)

	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/mcp/tokens",
		strings.NewReader(`{"name":"fixture","permissions":["crypto.sweep.runs.list"]}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "Permission is unknown")

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/mcp/oauth/requests/fixture/decision",
		strings.NewReader(`{"approve":true,"permissions":["crypto.sweep.runs.list"]}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "Unknown operation permission")
}
