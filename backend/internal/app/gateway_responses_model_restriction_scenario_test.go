package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/apikey/testkit"
	"github.com/TokenFlux/TokenRouter/internal/config"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	gatewaytestkit "github.com/TokenFlux/TokenRouter/internal/gateway/testkit"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
)

// Responses selection and billing share provider identifiers. Each loopback
// server captures the actual model independently of the identifier passed to
// transport. This detects a stale selection or a request sent to a different
// provider from the one charged for the completion.

// TestResponsesPlanModelRestrictionHTTP fixes group, priority and session for
// each request. A single plan affecting the first provider's mapped X leaves
// Y eligible there, with exactly one upstream attempt and settlement per call.
func TestResponsesPlanModelRestrictionHTTP(t *testing.T) {
	const (
		primary = int64(31)
		backup  = int64(32)
		modelX  = "gpt-5.4"
		modelY  = "gpt-5.2"
	)
	for _, tc := range []struct {
		name      string // behavior observed through the public Responses handler
		requested string // incoming model or alias
		actual    string // model expected on the receiving server
		restrict  bool   // supply plan A for the primary provider and X
		want      int64  // provider expected to receive the only HTTP attempt
	}{
		{"unrestricted alias", "client-x", modelX, false, primary},
		{"restricted alias", "client-x", modelX, true, backup},
		{"unaffected model", modelY, modelY, true, primary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const groupID int64 = 42
			group := &routing.Group{ID: groupID, Hydrated: true, Name: "G", Status: "active", RateMultiplier: 1,
				AllowedProtocols: []protocol.ProtocolID{protocol.ProtocolOpenAIResponses}}
			cfg := &config.Config{}
			cfg.Billing.FallbackRateMultiplier = 1
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.Failover.MaxProviderSwitches = 1
			providers := &mixedHTTPProviders{}
			seen := make(chan string, 8)
			for priority, id := range []int64{primary, backup} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					select {
					case seen <- fmt.Sprintf("%d %s %s", id, r.URL.Path, gjson.GetBytes(body, "model").String()):
					default:
						http.Error(w, "too many attempts", http.StatusBadRequest)
						return
					}
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					mixedUpstreamResponse(w, r, false)
				}))
				t.Cleanup(server.Close)
				record := &provider.Record{ID: id, Name: fmt.Sprint(id), Platform: provider.PlatformOpenAI,
					Type: provider.ProviderTypeAPIKey, Status: "active", Schedulable: true, Priority: priority + 1,
					Concurrency: 1, GroupIDs: []int64{groupID}, Credentials: map[string]any{
						"api_key": "fixture-key", "base_url": server.URL,
						"model_mapping": map[string]any{"client-x": modelX, modelY: modelY},
					}}
				if tc.restrict && id == primary {
					record.Extra = map[string]any{"plan_model_restrictions": map[string]any{
						"plan-A": map[string]any{"upstream_model": modelX, "restricted": true},
					}}
				}
				providers.values = append(providers.values, *gatewayprovider.NewExecutionProvider(record))
			}
			transport := &mixedHTTPTransport{}
			source, choices, credentials := newOpenAIExecutionAndSelectionFixture(providers, nil, cfg, nil, nil,
				nil, transport, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			logs := &gatewaytestkit.UsageLogStore{Inserted: true}
			funds := &gatewaytestkit.SettlementStore{}
			recording := gatewaytestkit.NewRecording(logs, funds, nil, false)
			recording.Options.DefaultMultiplier = 1
			source.Recorder = recording.Core(nil, true)
			user := &identity.User{ID: 7, Balance: 100, Status: "active"}
			keys := testkit.NewService(&openCodeHTTPKeys{key: &apikey.APIKey{
				ID: 8, Key: "fixture-client", Status: "active", UserID: user.ID, User: user,
				GroupID: &group.ID, Group: group, BillingMode: "balance",
			}}, nil, nil, nil, nil, nil, cfg)
			t.Cleanup(keys.Stop)
			eligibility := newBillingEligibilityFixture(cfg)
			t.Cleanup(eligibility.Stop)
			resources := provideOpenAIHTTPResources(scheduler.NewConcurrencyService(nil), cfg)
			endpoints := newGatewayHTTPEndpoints(gatewayHTTPFixtureInput{
				Source: source, Credentials: credentials, Choices: choices, Config: cfg, Keys: keys,
				Funding: newFundingAdmissionFixture(eligibility, cfg), Concurrency: resources.Concurrency, MaxSwitches: 1,
			})
			auth := provideAPIKeyAuth(keys, nil, cfg)
			guards := gatewayhttp.NewRouteGuards(legacyRouteMiddleware(auth, keys, nil, nil, nil, cfg))
			router := gin.New()
			const path = "/v1/responses"
			router.POST(path, gin.HandlerFunc(auth), guards.RequireGroupClientProtocol(
				protocol.ProtocolOpenAIResponses, gatewayhttp.GroupClientProtocolErrorOpenAI), endpoints.Responses)
			body := fmt.Sprintf(`{"model":%q,"input":"hello","stream":false}`, tc.requested)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer fixture-client")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("session_id", "plan-model-fixture")
			out := httptest.NewRecorder()
			router.ServeHTTP(out, req)
			require.Equal(t, http.StatusOK, out.Code, out.Body.String())
			require.Len(t, seen, 1)
			exchange := <-seen
			t.Logf("received=%s attempts=%v usage_writes=%d settlements=%d", exchange, transport.calls(), logs.Calls, funds.Calls)
			require.Equal(t, fmt.Sprintf("%d %s %s", tc.want, path, tc.actual), exchange)
			require.Equal(t, []int64{tc.want}, transport.calls())
			require.Equal(t, 1, logs.Calls)
			require.Equal(t, 1, funds.Calls)
			require.Equal(t, tc.want, logs.LastLog.ProviderID)
			require.Equal(t, 10, logs.LastLog.InputTokens)
			require.Equal(t, 3, logs.LastLog.OutputTokens)
		})
	}
}
