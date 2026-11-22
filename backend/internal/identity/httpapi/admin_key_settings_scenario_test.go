package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/audit"
	audithttp "github.com/TokenFlux/TokenRouter/internal/audit/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	routinghttp "github.com/TokenFlux/TokenRouter/internal/routing/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// admin_key_settings_scenario_test.go exercises admin_auth.go, routes_settings.go
// and admin_key_settings.go through the registered settings routes. Locally signed
// sessions and a mutable settings store expose the admitted principal, the public
// response and the effects of key rotation on the next request.

// managementKeyStore keeps runtime settings for sequential management HTTP requests.
type managementKeyStore map[string]string

// managementAccounts supplies current users for administrator JWT and key admission.
type managementAccounts map[int64]*identity.User

// managementKeyRoutes serves each request with the same account and settings state.
type managementKeyRoutes struct {
	auth         gin.HandlerFunc                  // real administrator admission
	handler      *httpapi.AdminKeySettingsHandler // registered settings handlers
	actor        identity.Principal               // last admitted principal
	cacheControl string                           // last response caching directive
}

// countedKeyHandlers observes entry into each business handler after route authorization.
type countedKeyHandlers struct {
	handler *httpapi.AdminKeySettingsHandler // actual settings endpoint implementation
	calls   int                              // business handlers entered
}

// managementAudit receives records from the audit writer for one local request.
type managementAudit struct {
	audit.AuditLogRepository                   // unused audit storage methods
	logs                     []*audit.AuditLog // events flushed after dispatch
}

// GetAdminAPIKey delegates an admitted status read to the production handler.
func (h *countedKeyHandlers) GetAdminAPIKey(c *gin.Context) {
	h.calls++
	h.handler.GetAdminAPIKey(c)
}

// RegenerateAdminAPIKey delegates an admitted rotation to the production handler.
func (h *countedKeyHandlers) RegenerateAdminAPIKey(c *gin.Context) {
	h.calls++
	h.handler.RegenerateAdminAPIKey(c)
}

// DeleteAdminAPIKey delegates an admitted deletion to the production handler.
func (h *countedKeyHandlers) DeleteAdminAPIKey(c *gin.Context) {
	h.calls++
	h.handler.DeleteAdminAPIKey(c)
}

// BatchInsert collects records for inspection after the single writer stops.
func (a *managementAudit) BatchInsert(_ context.Context, logs []*audit.AuditLog) (int64, error) {
	a.logs = append(a.logs, logs...)
	return int64(len(logs)), nil
}

// GetValue reads the setting currently visible to both admission and the handler.
func (s managementKeyStore) GetValue(_ context.Context, key string) (string, error) {
	value, ok := s[key]
	if !ok {
		return "", settings.ErrSettingNotFound
	}
	return value, nil
}

// GetMultiple rejects batch reads outside the management credential settings contract.
func (s managementKeyStore) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected batch settings read")
}

// Set stores a regenerated credential for subsequent requests through the router.
func (s managementKeyStore) Set(_ context.Context, key, value string) error {
	s[key] = value
	return nil
}

// Delete removes the stored credential observed by subsequent management requests.
func (s managementKeyStore) Delete(_ context.Context, key string) error {
	delete(s, key)
	return nil
}

// GetByID reads the current account corresponding to the verified session.
func (a managementAccounts) GetByID(_ context.Context, id int64) (*identity.User, error) {
	if user := a[id]; user != nil {
		return user, nil
	}
	return nil, identity.ErrUserNotFound
}

// GetFirstAdmin returns the active administrator represented by the fixture key.
func (a managementAccounts) GetFirstAdmin(context.Context) (*identity.User, error) {
	return a[1], nil
}

// request serves one standard management route and checks the actual audit event.
// Each request has a separate audit writer so its drained records can be matched
// to that request even when several consecutive requests use the same credential.
func (r *managementKeyRoutes) request(t *testing.T, role, method, target, header, credential string, status int) map[string]any {
	t.Helper()
	r.actor = identity.Principal{}
	business := &countedKeyHandlers{handler: r.handler}
	captured := &managementAudit{}
	service := audit.NewAuditLogService(captured, nil)
	router := gin.New()
	group := router.Group("/api/v1/admin", r.auth)
	group.Use(gin.HandlerFunc(audithttp.NewAuditLogMiddleware(service, audit.NewRedactor(nil))))
	group.Use(func(c *gin.Context) {
		r.actor, _ = authctx.GetPrincipal(c)
		c.Next()
	})
	httpapi.RegisterIdentitySettingsRoutes(group.Group("/settings"), business)
	catalog := routinghttp.NewProtocolCatalogHandler(nil)
	group.GET("/protocol-capabilities", func(c *gin.Context) {
		business.calls++
		catalog(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+target, nil)
	require.NoError(t, err)
	req.Header.Set(header, credential)
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	raw, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	// Join HTTP handlers before draining queued audit entries and inspecting fixture state shared with the test goroutine.
	server.Close()
	service.Start()
	service.Stop()
	require.NoError(t, readErr)
	require.NoError(t, closeErr)
	r.cacheControl = resp.Header.Get("Cache-Control")
	calls := business.calls
	t.Logf("role=%s entry=%s %s http=%d calls=%d actor=%d/%s audit=%d cache_control=%q",
		role, method, target, resp.StatusCode, calls, r.actor.UserID, r.actor.CredentialKind, len(captured.logs), r.cacheControl)
	require.Equal(t, status, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	if status == http.StatusOK {
		require.Equal(t, 1, calls)
	} else {
		require.Zero(t, calls)
		require.NotContains(t, body, "data")
	}
	if status == http.StatusOK || (role == "management_key" && status == http.StatusForbidden) {
		require.Equal(t, int64(1), r.actor.UserID)
		require.Equal(t, identity.RoleAdmin, r.actor.Role)
	} else {
		require.Zero(t, r.actor)
	}
	if r.actor.UserID == 0 || target == "/api/v1/admin/protocol-capabilities" {
		require.Empty(t, captured.logs)
	} else {
		require.Len(t, captured.logs, 1)
		event := captured.logs[0]
		require.Equal(t, &r.actor.UserID, event.ActorUserID)
		require.Equal(t, identity.RoleAdmin, event.ActorRole)
		require.Equal(t, r.actor.CredentialKind, event.AuthMethod)
		require.Equal(t, target, event.Path)
		require.Equal(t, method, event.Method)
		require.Equal(t, status, event.StatusCode)
		require.Equal(t, map[string]string{"GET": "admin.admin_api_key.read", "POST": "admin.admin_api_key.regenerate", "DELETE": "admin.admin_api_key.delete"}[method], event.Action)
	}
	return body
}

// TestF06AdminKeyRoutes uses signed administrator and user sessions against the
// registered key settings handlers. It checks the principal at the audit boundary,
// the status projection and stored credential across rejected and accepted changes.
func TestF06AdminKeyRoutes(t *testing.T) {
	const path = "/api/v1/admin/settings/admin-api-key"
	const seed = "example-management-credential"
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	options := identity.SessionOptions{
		Secret: "local-fixture-signing-secret", AccessTokenExpireMinutes: 15,
		Now: func() time.Time { return now },
	}
	sessions := identity.NewSessionService(options, nil, nil, nil, nil)
	accounts := managementAccounts{
		1: {ID: 1, Role: identity.RoleAdmin, Status: identity.StatusActive, TokenVersionResolved: true},
		2: {ID: 2, Role: identity.RoleUser, Status: identity.StatusActive, TokenVersionResolved: true},
	}
	admin, err := sessions.GenerateAccessToken(accounts[1], "admin-session", "")
	require.NoError(t, err)
	user, err := sessions.GenerateAccessToken(accounts[2], "user-session", "")
	require.NoError(t, err)
	store := managementKeyStore{identity.SettingKeyAdminAPIKey: seed}
	runtime := identity.NewRuntimeSettings(store, settings.ErrSettingNotFound)
	routes := &managementKeyRoutes{auth: httpapi.AdminAuth(sessions, accounts, runtime, nil), handler: httpapi.NewAdminKeySettingsHandler(runtime)}

	result := routes.request(t, "user", http.MethodDelete, path, "Authorization", "Bearer "+user, http.StatusForbidden)
	require.Equal(t, "FORBIDDEN", result["code"])
	result = routes.request(t, "gateway_key", http.MethodDelete, path, "x-api-key", "example-gateway-key", http.StatusUnauthorized)
	require.Equal(t, "INVALID_ADMIN_KEY", result["code"])
	require.Equal(t, seed, store[identity.SettingKeyAdminAPIKey])

	result = routes.request(t, "admin_session", http.MethodGet, path, "Authorization", "Bearer "+admin, http.StatusOK)
	require.Equal(t, map[string]any{"exists": true, "masked_key": "example-ma...tial"}, result["data"])
	require.Equal(t, int64(1), routes.actor.UserID)
	require.Equal(t, "jwt", routes.actor.CredentialKind)

	result = routes.request(t, "admin_session", http.MethodPost, path+"/regenerate", "Authorization", "Bearer "+admin, http.StatusOK)
	replacement := store[identity.SettingKeyAdminAPIKey]
	require.NotEmpty(t, replacement)
	require.NotEqual(t, seed, replacement)
	require.Equal(t, map[string]any{"key": replacement}, result["data"])
	require.Equal(t, "no-store", routes.cacheControl)
	require.Equal(t, "admin-session", routes.actor.SessionID)
	require.Equal(t, "jwt", routes.actor.CredentialKind)
	require.Equal(t, int64(1), routes.actor.UserID)
	result = routes.request(t, "management_key", http.MethodDelete, path, "x-api-key", seed, http.StatusUnauthorized)
	require.Equal(t, "INVALID_ADMIN_KEY", result["code"])
	require.Equal(t, replacement, store[identity.SettingKeyAdminAPIKey])

	routes.request(t, "admin_session", http.MethodDelete, path, "Authorization", "Bearer "+admin, http.StatusOK)
	require.NotContains(t, store, identity.SettingKeyAdminAPIKey)
	routes.request(t, "management_key", http.MethodGet, path, "x-api-key", replacement, http.StatusUnauthorized)
	result = routes.request(t, "admin_session", http.MethodGet, path, "Authorization", "Bearer "+admin, http.StatusOK)
	require.Equal(t, map[string]any{"exists": false, "masked_key": ""}, result["data"])

	// Advance the signing and validation clock to the exact expiry boundary so admission is independent of scheduling delays.
	now = now.Add(15 * time.Minute)
	result = routes.request(t, "admin_session", http.MethodPost, path+"/regenerate", "Authorization", "Bearer "+admin, http.StatusUnauthorized)
	require.Equal(t, "TOKEN_EXPIRED", result["code"])
	require.NotContains(t, store, identity.SettingKeyAdminAPIKey)
}

// TestF06SiteKeyLifecycleBoundary presents the same valid site key to an ordinary
// query and its three credential management routes. Each denied request records
// its authenticated actor without entering the business handler or changing settings.
func TestF06SiteKeyLifecycleBoundary(t *testing.T) {
	const seed = "example-site-management"
	const path = "/api/v1/admin/settings/admin-api-key"
	store := managementKeyStore{identity.SettingKeyAdminAPIKey: seed}
	runtime := identity.NewRuntimeSettings(store, settings.ErrSettingNotFound)
	accounts := managementAccounts{1: {ID: 1, Role: identity.RoleAdmin, Status: identity.StatusActive}}
	routes := &managementKeyRoutes{auth: httpapi.AdminAuth(nil, accounts, runtime, nil), handler: httpapi.NewAdminKeySettingsHandler(runtime)}
	result := routes.request(t, "management_key", http.MethodGet, "/api/v1/admin/protocol-capabilities", "x-api-key", seed, http.StatusOK)
	require.NotEmpty(t, result["data"])
	require.Equal(t, "admin_api_key", routes.actor.CredentialKind)
	require.Empty(t, routes.actor.SessionID)
	for _, tc := range []struct {
		method string // registered HTTP method
		target string // credential lifecycle endpoint
	}{
		{http.MethodGet, path},
		{http.MethodPost, path + "/regenerate"},
		{http.MethodDelete, path},
	} {
		result := routes.request(t, "management_key", tc.method, tc.target, "x-api-key", seed, http.StatusForbidden)
		require.Equal(t, "FORBIDDEN", result["code"])
		require.Equal(t, "admin_api_key", routes.actor.CredentialKind)
		require.Equal(t, seed, store[identity.SettingKeyAdminAPIKey])
	}
}

// TestF06AdminKeyStatusMask reads stored display values across the preview
// boundary. A short credential has a presence marker, while longer values
// retain the existing prefix and suffix for operator recognition.
func TestF06AdminKeyStatusMask(t *testing.T) {
	store := managementKeyStore{}
	accounts := managementAccounts{1: {ID: 1, Role: identity.RoleAdmin, Status: identity.StatusActive, TokenVersionResolved: true}}
	sessions := identity.NewSessionService(identity.SessionOptions{Secret: "local-status-signing-fixture", AccessTokenExpireMinutes: 15}, nil, nil, nil, nil)
	browser, err := sessions.GenerateAccessToken(accounts[1], "status-session", "")
	require.NoError(t, err)
	runtime := identity.NewRuntimeSettings(store, settings.ErrSettingNotFound)
	routes := &managementKeyRoutes{auth: httpapi.AdminAuth(sessions, accounts, runtime, nil), handler: httpapi.NewAdminKeySettingsHandler(runtime)}
	for _, tc := range []struct {
		value string // stored example value
		mask  string // expected status projection
	}{
		{"a", "****"},
		{"12345678901234", "****"},
		{"123456789012345", "1234567890...2345"},
	} {
		store[identity.SettingKeyAdminAPIKey] = tc.value
		result := routes.request(t, "admin_session", http.MethodGet, "/api/v1/admin/settings/admin-api-key", "Authorization", "Bearer "+browser, http.StatusOK)
		require.Equal(t, map[string]any{"exists": true, "masked_key": tc.mask}, result["data"])
		require.Equal(t, tc.value, store[identity.SettingKeyAdminAPIKey])
	}
}
