package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/audit"
	audithttp "github.com/TokenFlux/TokenRouter/internal/audit/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	identityhttp "github.com/TokenFlux/TokenRouter/internal/identity/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	control "github.com/TokenFlux/TokenRouter/internal/mcp"
	"github.com/TokenFlux/TokenRouter/internal/mcp/rediscache"
	providerhttp "github.com/TokenFlux/TokenRouter/internal/provider/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/server/adminapi"
)

// management_authorization_scenario_test.go checks transport.go, execute.go and
// output.go against the registered identity settings routes. Each SDK call has
// isolated settings and a drained audit queue so discovery, browser continuations,
// rejected calls and completed HTTP operations have independently observable effects.

// keyOperationStore supplies only the settings operations exercised by admitted calls.
type keyOperationStore struct {
	identity.RuntimeSettingsStore        // unused settings repository operations
	value                         string // current synthetic management key
	reads                         int    // status reads completed
	deletes                       int    // credential removals completed
}

// operationAuditStore collects the real management middleware's emitted audit events.
type operationAuditStore struct {
	audit.AuditLogRepository                   // unused audit persistence methods
	logs                     []*audit.AuditLog // events drained after dispatch
}

// fixedApplication represents the result of validating one local OAuth credential.
type fixedApplication struct {
	identity.MCPOAuthResolver                   // stable browser grant lookup is outside this fixture
	grant                     identity.MCPToken // immutable synthetic application grant
}

// GetValue observes a business read of the registered key status endpoint.
func (s *keyOperationStore) GetValue(_ context.Context, key string) (string, error) {
	if key != identity.SettingKeyAdminAPIKey {
		return "", errors.New("unexpected settings read")
	}
	s.reads++
	return s.value, nil
}

// Delete removes the fixture credential and counts the completed business mutation.
func (s *keyOperationStore) Delete(_ context.Context, key string) error {
	if key != identity.SettingKeyAdminAPIKey {
		return errors.New("unexpected settings deletion")
	}
	s.value = ""
	s.deletes++
	return nil
}

// BatchInsert retains the writer's entries until the test checks their attribution.
func (s *operationAuditStore) BatchInsert(_ context.Context, entries []*audit.AuditLog) (int64, error) {
	s.logs = append(s.logs, entries...)
	return int64(len(entries)), nil
}

// Authenticate returns a fresh snapshot of the application's fixed permissions.
// Unknown credentials return the normal admission error. Database and protocol
// checks for acquisition of this credential belong to the OAuth integration suite.
func (a fixedApplication) Authenticate(_ context.Context, credential string) (*identity.MCPToken, error) {
	if credential != "example-oauth-access" {
		return nil, identity.ErrMCPTokenInvalid
	}
	return &a.grant, nil
}

// TestF06MCPToolAuthorization calls the four published tools with fixed manual
// and application grants. It compares each result with actual settings operations
// and the credential source, owner and operation recorded by the audit writer.
func TestF06MCPToolAuthorization(t *testing.T) {
	const (
		list            = "settings.admin_api_key.list"
		remove          = "settings.admin_api_key.delete"
		browser         = "mcp.oauth.status"
		connectProvider = "antigravity.oauth.auth_url.create"
		listProviders   = "providers.list"
		seed            = "example-management-credential"
	)
	for _, tc := range []struct {
		name    string // individual policy example
		source  string // grant source discriminator
		tool    string // SDK tool name
		op      string // requested operation identifier
		status  string // expected tool outcome
		reads   int    // expected business reads
		deletes int    // expected business writes
	}{
		{"manual_discovery", "", "search_operations", "", "search", 0, 0},
		{"oauth_discovery", "oauth", "search_operations", "", "search", 0, 0},
		{"describe_allowed", "", "describe_operation", remove, "describe", 0, 0},
		{"describe_denied", "oauth", "describe_operation", remove, "failed", 0, 0},
		{"manual_read", "", "read_operation", list, "succeeded", 1, 0},
		{"oauth_read", "oauth", "read_operation", list, "succeeded", 1, 0},
		{"read_rejects_write", "", "read_operation", remove, "failed", 0, 0},
		{"grant_rejects_write", "oauth", "execute_operation", remove, "failed", 0, 0},
		{"confirmed_manual_write", "", "execute_operation", remove, "succeeded", 0, 1},
		{"manual_write", "", "execute_operation", remove, "succeeded", 0, 1},
		{"follow_up_granted", "", "execute_operation", connectProvider, "interaction_required", 0, 0},
		{"follow_up_ungranted", "oauth", "execute_operation", connectProvider, "interaction_required", 0, 0},
		{"browser_continuation", "oauth", "read_operation", browser, "interaction_required", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := &accountFixture{}
			accounts.role.Store(identity.RoleAdmin)
			// Both sources use the same numeric ID. Independent permissions and audit attribution must survive this overlap through internal HTTP dispatch.
			manual := identity.MCPToken{ID: 1, UserID: 1, Enabled: true, SkipBrowserConfirmation: true, Prefix: "manual-example", Permissions: []string{list, remove, connectProvider, listProviders}}
			app := identity.MCPToken{ID: 1, UserID: 1, Enabled: true, SkipBrowserConfirmation: true, Source: "oauth", Prefix: "oauth:1", Permissions: []string{list, browser, connectProvider}}
			confirmed := tc.name == "confirmed_manual_write"
			var state control.StateStore
			if confirmed {
				manual.SkipBrowserConfirmation = false
				client := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
				t.Cleanup(func() { require.NoError(t, client.Close()) })
				state = rediscache.NewState(client, nil)
			}
			credential := "tr_mcp_" + strings.Repeat("a", 43)
			tokens := identity.NewMCPTokenService(&tokenFixture{token: manual, digest: identity.MCPTokenDigest(credential)}, accounts, nil)
			tokens.SetOAuth(fixedApplication{grant: app})
			grant := manual
			if tc.source == "oauth" {
				grant = app
				credential = "example-oauth-access"
			}
			store := &keyOperationStore{value: seed}
			captured := &operationAuditStore{}
			service := audit.NewAuditLogService(captured, nil)
			auditor := gin.HandlerFunc(audithttp.NewAuditLogMiddleware(service, audit.NewRedactor(nil)))
			admin := identityhttp.MCPAdminAuth(tokens, func(c *gin.Context) {
				if c.GetHeader("Authorization") != "Bearer fixture-owner-session" {
					c.AbortWithStatus(http.StatusUnauthorized)
					return
				}
				authctx.SetPrincipal(c, identity.Principal{UserID: 1, Role: identity.RoleAdmin, CredentialKind: "jwt", SessionID: "owner-session"}, 1, "")
				c.Next()
			})
			router := gin.New()
			group := router.Group("/api/v1/admin/settings", admin, auditor)
			identityhttp.RegisterIdentitySettingsRoutes(group, identityhttp.NewAdminKeySettingsHandler(identity.NewRuntimeSettings(store, nil)))
			// A browser handoff may suggest a separately granted read. Neither provider endpoint should execute during the tool call.
			providerCalls := 0
			adminGroup := router.Group("/api/v1/admin", admin, auditor)
			for _, id := range []string{connectProvider, listProviders} {
				adminapi.Register(adminGroup, providerhttp.ManagementOperations[id], func(c *gin.Context) {
					providerCalls++
					c.Status(http.StatusNoContent)
				})
			}
			handler := NewHandler(tokens, state, nil, accounts, nil, Options{})
			require.NoError(t, handler.Mount(router, admin, auditor))
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			client := mcp.NewClient(&mcp.Implementation{Name: "contract", Version: "1"}, nil)
			session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
				Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{credential}},
				MaxRetries: -1, DisableStandaloneSSE: true,
			}, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, session.Close()) })
			args := map[string]any{}
			if tc.op != "" {
				args["operation_id"] = tc.op
			}
			sdk := &mcpFixture{session: session}
			result, data := sdk.invoke(t, tc.tool, args)
			if confirmed {
				require.False(t, result.IsError)
				require.Equal(t, "confirmation_required", data["status"])
				require.Zero(t, store.deletes)
				require.Equal(t, seed, store.value)
				t.Logf("role=admin credential=manual entry=%s operation=%s status=confirmation_required isError=false calls=%d", tc.tool, tc.op, store.deletes)
				id := jsonTestValue[string](t, data["confirmation_id"])
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/admin/mcp/confirmations/"+id+"/approve", nil)
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer fixture-owner-session")
				response, err := server.Client().Do(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusOK, response.StatusCode)
				args["confirmation_id"] = id
				result, data = sdk.invoke(t, tc.tool, args)
			}
			server.Close()
			// Flush every event emitted during this call before checking the per-operation audit count and the authenticated actor.
			service.Start()
			service.Stop()
			calls := store.reads + store.deletes + providerCalls
			var actorID int64
			if len(captured.logs) > 0 {
				require.NotNil(t, captured.logs[0].ActorUserID)
				actorID = *captured.logs[0].ActorUserID
			}
			t.Logf("role=admin credential=%s entry=%s operation=%s isError=%t calls=%d actor=%d audit=%d",
				grant.Prefix, tc.tool, tc.op, result.IsError, calls, actorID, len(captured.logs))
			require.Equal(t, tc.status == "failed", result.IsError)
			require.Zero(t, providerCalls)
			require.Equal(t, tc.reads, store.reads)
			require.Equal(t, tc.deletes, store.deletes)
			events := captured.logs
			if confirmed {
				require.Len(t, events, 2)
				approval := events[0]
				require.Equal(t, "jwt", approval.AuthMethod)
				require.Equal(t, &grant.UserID, approval.ActorUserID)
				require.Equal(t, tc.op, approval.Extra["mcp_operation_id"])
				require.Equal(t, http.StatusOK, approval.StatusCode)
				t.Logf("entry=confirmation_approval actor=%d/jwt http=%d audit=1", *approval.ActorUserID, approval.StatusCode)
				events = events[1:]
			}
			require.Len(t, events, calls)
			if calls > 0 {
				event := events[0]
				require.Equal(t, grant.UserID, actorID)
				require.Equal(t, identity.RoleAdmin, event.ActorRole)
				require.Equal(t, "mcp_token", event.AuthMethod)
				require.Equal(t, grant.Prefix, event.CredentialMasked)
				require.Equal(t, grant.ID, event.Extra["mcp_token_id"])
				require.Equal(t, grant.Source, event.Extra["mcp_source"])
				require.Equal(t, tc.op, event.Extra["mcp_operation_id"])
				require.Equal(t, http.StatusOK, event.StatusCode)
			}
			if tc.deletes == 1 {
				require.Empty(t, store.value)
			} else {
				require.Equal(t, seed, store.value)
			}
			switch tc.status {
			case "search":
				var ids []string
				for _, item := range jsonTestValue[[]any](t, data["items"]) {
					ids = append(ids, jsonTestValue[string](t, jsonTestValue[map[string]any](t, item)["operation_id"]))
				}
				require.ElementsMatch(t, grant.Permissions, ids)
			case "describe":
				require.Equal(t, remove, data["operation_id"])
				require.Equal(t, false, data["read_only"])
				require.Equal(t, false, data["requires_confirmation"])
			case "interaction_required":
				require.Equal(t, tc.status, data["status"])
				if tc.op == browser {
					require.Equal(t, "/admin/mcp", data["url"])
				} else {
					require.Equal(t, "/admin/providers", data["url"])
					if grant.Allows(listProviders) {
						require.Equal(t, listProviders, data["follow_up_operation"])
					} else {
						require.NotContains(t, data, "follow_up_operation")
						require.NotContains(t, data["message"], listProviders)
					}
				}
				require.NotContains(t, data, "data")
			case "failed":
				require.Equal(t, tc.status, data["status"])
				require.NotContains(t, data, "data")
			case "succeeded":
				require.Equal(t, tc.status, data["status"])
				require.Equal(t, float64(http.StatusOK), data["http_status"])
				payload := jsonTestValue[map[string]any](t, data["data"])["data"]
				if tc.reads == 1 {
					require.Equal(t, map[string]any{"exists": true, "masked_key": "example-ma...tial"}, payload)
				} else {
					require.Equal(t, map[string]any{"message": "Admin API key deleted"}, payload)
				}
			}
			require.NotContains(t, jsonTestValue[*mcp.TextContent](t, result.Content[0]).Text, seed)
		})
	}
}

// TestF06MCPCredentialAdmission changes the persisted state of a previously
// admitted manual token. The next ordinary request observes each rejection at
// transport admission before any tool can perform a fixture mutation.
func TestF06MCPCredentialAdmission(t *testing.T) {
	for _, state := range []string{"expired", "revoked", "disabled", "owner_demoted"} {
		t.Run(state, func(t *testing.T) {
			f := newMCPFixture(t)
			past := time.Unix(1, 0).UTC()
			// Set the persisted state under the same fixture lock used by transport reads before sending the next ordinary request.
			f.repo.mu.Lock()
			switch state {
			case "expired":
				f.repo.token.ExpiresAt = &past
			case "revoked":
				f.repo.token.RevokedAt = &past
			case "disabled":
				f.repo.token.Enabled = false
			case "owner_demoted":
				f.accounts.role.Store(identity.RoleUser)
			}
			f.repo.mu.Unlock()
			// Send a valid, granted fixture write so only transport authentication determines whether the mounted business handler can run.
			body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_operation","arguments":{"operation_id":"fixture.create","body":{}}}}`)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/mcp", body)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", "Bearer "+f.credential)
			response, err := f.server.Client().Do(req)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			require.Zero(t, f.writes.Load())
			t.Logf("credential=manual state=%s entry=POST /mcp http=%d writes=%d actor=none", state, response.StatusCode, f.writes.Load())
		})
	}
}
