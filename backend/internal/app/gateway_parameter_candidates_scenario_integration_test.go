//go:build integration

package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/apikey"
	keyhttp "github.com/TokenFlux/TokenRouter/internal/apikey/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/apikey/testkit"
	"github.com/TokenFlux/TokenRouter/internal/config"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewaypg "github.com/TokenFlux/TokenRouter/internal/gateway/postgres"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	"github.com/TokenFlux/TokenRouter/internal/gateway/searchtools"
	"github.com/TokenFlux/TokenRouter/internal/gateway/session"
	gatewaytestkit "github.com/TokenFlux/TokenRouter/internal/gateway/testkit"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/server/middleware"
)

// strictCandidateBusySlots denies every provider slot and wait queue admission.
type strictCandidateBusySlots struct {
	fakeConcurrencyCache // standard fixture user slot and cleanup behavior
}

// AcquireProviderSlot reports an occupied upstream slot for every candidate.
func (strictCandidateBusySlots) AcquireProviderSlot(context.Context, int64, int, string) (bool, error) {
	return false, nil
}

// IncrementProviderWaitCount reports a full queue without registering a waiter.
func (strictCandidateBusySlots) IncrementProviderWaitCount(context.Context, int64, int) (bool, error) {
	return false, nil
}

// TestGatewayStrictParameterCandidates verifies actual upstream receipts
// after strict routing through the three client protocols. It also proves
// that exhausted and immutable-bound requests have zero HTTP sends.
func TestGatewayStrictParameterCandidates(t *testing.T) {
	db := qualityTestDatabase(t)
	entClient := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	store := provideSettingsStore(entClient)
	parameters := requestpolicy.New(store)
	cohorts := gatewaypg.NewCompatibilityCohorts(db)
	const alias = "strict-fixture-alias"
	pairs := []struct {
		path string              // client HTTP endpoint
		wire protocol.ProtocolID // client protocol identity
	}{
		{requestpolicy.EndpointMessages, protocol.ProtocolAnthropicMessages},
		{requestpolicy.EndpointChat, protocol.ProtocolOpenAIChatCompletions},
		{requestpolicy.EndpointResponses, protocol.ProtocolOpenAIResponses},
	}
	policy := requestpolicy.Config{Mode: requestpolicy.ModeStrict}
	for _, pair := range pairs {
		policy.Rules = append(policy.Rules, requestpolicy.Rule{
			Entry: requestpolicy.Entry{EntryGroupID: parameterFixtureGroup, Endpoint: pair.path, RequestedModel: alias},
			Mode:  requestpolicy.ModeStrict, StrictPercent: 100,
		})
	}
	_, err := parameters.Update(t.Context(), policy)
	require.NoError(t, err)

	for _, tc := range []struct {
		name        string              // expected routing outcome
		pair        int                 // index of the client entry
		outPath     string              // actual upstream endpoint
		outProtocol protocol.ProtocolID // actual upstream encoding
		second      *bool               // nil leaves candidate B unverified
		previous    bool                // bind previous response to candidate A
		status      int                 // expected client HTTP status
	}{
		{"messages/pass", 0, requestpolicy.EndpointMessages, protocol.ProtocolAnthropicMessages, new(true), false, http.StatusOK},
		{"chat/pass", 1, requestpolicy.EndpointChat, protocol.ProtocolOpenAIChatCompletions, new(true), false, http.StatusOK},
		{"responses/bridge", 2, requestpolicy.EndpointMessages, protocol.ProtocolAnthropicMessages, new(true), false, http.StatusOK},
		{"responses/authorized-fallback", 2, requestpolicy.EndpointResponses, protocol.ProtocolOpenAIResponses, new(true), false, http.StatusOK},
		{"responses/all-rejected", 2, requestpolicy.EndpointResponses, protocol.ProtocolOpenAIResponses, new(false), false, http.StatusBadRequest},
		{"responses/unknown", 2, requestpolicy.EndpointResponses, protocol.ProtocolOpenAIResponses, nil, false, http.StatusServiceUnavailable},
		{"responses/no-capacity", 2, requestpolicy.EndpointResponses, protocol.ProtocolOpenAIResponses, new(true), false, http.StatusServiceUnavailable},
		{"responses/bound-rejected", 2, requestpolicy.EndpointResponses, protocol.ProtocolOpenAIResponses, new(true), true, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair := pairs[tc.pair]
			require.NoError(t, parameters.Refresh(t.Context()))
			cfg := &config.Config{}
			cfg.Billing.FallbackRateMultiplier = 1
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			group := &routing.Group{
				ID: parameterFixtureGroup, Hydrated: true, Name: "strict-fixture", Status: routing.StatusActive, RateMultiplier: 1,
				AllowedProtocols: []protocol.ProtocolID{pair.wire},
			}
			if tc.outProtocol != pair.wire {
				group.ProtocolFallbacks = map[protocol.ProtocolID][]protocol.ProtocolID{pair.wire: {tc.outProtocol}}
			}
			if tc.pair == 1 {
				group.SchedulerType = routing.GroupSchedulerTypeAdvanced
			}
			origin := group
			if tc.name == "responses/authorized-fallback" {
				old := *group
				origin = &old
				group.ID++
				origin.ClaudeCodeOnly, origin.FallbackGroupID = true, &group.ID
			}
			serve := func(w http.ResponseWriter, r *http.Request) {
				// The receipt listener has already consumed the actual body.
				// Script the bridge's expected framing and assert it on the receipt below.
				body := `{"stream":false}`
				if tc.outProtocol != pair.wire {
					body = `{"stream":true}`
				}
				r.Body = io.NopCloser(strings.NewReader(body))
				writeParameterFixtureSuccess(t, w, r, []byte(body))
			}
			a := newResponsesCompatibilityUpstream(t, serve)
			b := newResponsesCompatibilityUpstream(t, serve)
			modelA, modelB := alias+"-wire-a", alias+"-wire-b"
			platform := provider.PlatformOpenAI
			if tc.outProtocol == protocol.ProtocolAnthropicMessages {
				platform = provider.PlatformAnthropic
			}
			pool := &mixedHTTPProviders{values: []gatewayprovider.ExecutionProvider{
				strictParameterHTTPProvider(parameterFixtureProvider, group.ID, 1, platform, a.url, alias, modelA, pair.wire, tc.outProtocol, new(false)),
				strictParameterHTTPProvider(parameterFixtureProvider+1, group.ID, 2, platform, b.url, alias, modelB, pair.wire, tc.outProtocol, tc.second),
			}}
			state := session.NewOpenAIWSStateStore(nil)
			fields := `,"temperature":0.45`
			if tc.previous {
				fields += `,"previous_response_id":"resp_strict_bound"`
				require.NoError(t, state.BindResponseProvider(t.Context(), group.ID, "resp_strict_bound", parameterFixtureProvider, time.Minute))
				require.NoError(t, state.BindHTTPResponseOwner(t.Context(), group.ID, "resp_strict_bound", parameterFixtureUser, parameterFixtureKey, time.Minute))
			}
			transport := provideHTTPUpstream(cfg)
			t.Cleanup(transport.CloseIdleConnections)
			var selectorConcurrency *scheduler.ConcurrencyService
			concurrency := scheduler.NewConcurrencyService(nil)
			if tc.name == "responses/no-capacity" {
				concurrency = scheduler.NewConcurrencyService(&strictCandidateBusySlots{})
				selectorConcurrency = concurrency
				cfg.Scheduling.AlternateProviderWait.MaxWaiting = 1
				cfg.Scheduling.AlternateProviderWait.TimeoutSeconds = 1
			}
			source, choices, credentials := newOpenAIExecutionAndSelectionFixture(pool, nil, cfg, nil, selectorConcurrency, nil, transport, nil, nil, nil, nil, nil, nil, nil, nil, nil, state, nil, nil)
			source.Recorder = gatewaytestkit.NewRecording(&gatewaytestkit.UsageLogStore{Inserted: true}, &gatewaytestkit.SettlementStore{}, nil, false).Core(nil, true)
			source.Responses.Cohorts = cohorts
			readers := &gatewayprovider.RuntimeReaders{Gateway: provideGatewaySettings(store)}
			messages := provideMessagesExecution(&provider.MessageCredentialSource{}, nil, transport, nil, nil, readers, nil,
				searchtools.NewEmulator(mixedHTTPNoSearch{}, nil, nil, nil, nil, nil), nil, nil, pool, &provider.DeferredService{}, cfg, nil, nil)
			billing := newBillingEligibilityFixture(cfg)
			t.Cleanup(billing.Stop)
			funding := newFundingAdmissionFixture(billing, cfg)
			user := &identity.User{ID: parameterFixtureUser, Balance: 100, Status: "active"}
			key := &apikey.APIKey{ID: parameterFixtureKey, Key: parameterFixtureToken, Status: "active", UserID: user.ID, User: user, GroupID: &origin.ID, Group: origin, BillingMode: "balance"}
			groups := fallbackGroupRepository{groups: map[int64]*routing.Group{origin.ID: origin, group.ID: group}}
			keys := testkit.NewService(&openCodeHTTPKeys{key: key}, fallbackUserRepository{user: user}, groups, nil, nil, nil, cfg)
			t.Cleanup(keys.Stop)
			endpoints := newGatewayHTTPEndpoints(gatewayHTTPFixtureInput{
				Source: source, Native: &gatewayhttp.UnifiedTextExecutor{OpenAI: source.Responses, Anthropic: messages},
				Choices: choices, Credentials: credentials, Config: cfg, Keys: keys,
				Funding: funding, ParameterPolicy: parameters,
				Concurrency: provideOpenAIHTTPResources(concurrency, cfg).Concurrency,
			})
			router := gin.New()
			router.Use(middleware.ClientRequestID(), gatewayhttp.TextRequestTiming(), gatewayhttp.InboundEndpointMiddleware())
			bind := func(c *gin.Context) {
				c.Set(string(keyhttp.ContextKeyAPIKey), key)
				c.Set("gateway_effective_key", key)
				authctx.SetPrincipal(c, identity.Principal{UserID: user.ID}, 0, "")
				ctx := requeststate.WithClientProtocol(requeststate.WithGroup(c.Request.Context(), group), pair.wire)
				c.Request = c.Request.WithContext(ctx)
			}
			endpoint := []gin.HandlerFunc{endpoints.Messages, endpoints.ChatCompletions, endpoints.Responses}[tc.pair]
			if origin != group {
				auth := provideAPIKeyAuth(keys, nil, cfg)
				mw := legacyRouteMiddleware(auth, keys, nil, nil, nil, cfg)
				mw.ClientGroupFallback = provideClientGroupFallbackResolver(keys, funding, nil, nil)
				guards := gatewayhttp.NewRouteGuards(mw)
				router.POST(pair.path, gin.HandlerFunc(auth), guards.RequireGroupClientProtocol(pair.wire, gatewayhttp.GroupClientProtocolErrorOpenAI), endpoint)
			} else {
				router.POST(pair.path, bind, endpoint)
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, pair.path, strings.NewReader(parameterPayload(pair.path, alias, false, fields)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+parameterFixtureToken)
			router.ServeHTTP(response, request)
			t.Logf("entry=%s status=%d a=%d b=%d", pair.path, response.Code, len(a.snapshot()), len(b.snapshot()))
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Empty(t, a.snapshot(), "rejected candidate received a request")
			switch tc.status {
			case http.StatusOK:
				received := b.snapshot()
				require.Len(t, received, 1)
				require.Equal(t, tc.outPath, received[0].path)
				require.Equal(t, modelB, gjson.GetBytes(received[0].body, "model").String())
				require.Equal(t, "0.45", gjson.GetBytes(received[0].body, "temperature").Raw)
				require.Equal(t, tc.outProtocol != pair.wire, gjson.GetBytes(received[0].body, "stream").Bool())
				if origin != group {
					assertParameterCohort(t, db, response.Header().Get("X-TokenRouter-Request-ID"), 1, "strict", "completed", "", pair.path, alias)
				}
				t.Logf("wire=%s model=%s temperature=%s", received[0].path, modelB, gjson.GetBytes(received[0].body, "temperature").Raw)
			case http.StatusBadRequest:
				require.Empty(t, b.snapshot())
				require.Contains(t, response.Body.String(), "temperature")
				assertParameterCohort(t, db, response.Header().Get("X-TokenRouter-Request-ID"), 1, "strict", "parameter_400", "candidate", pair.path, alias)
			default:
				require.Empty(t, b.snapshot())
				require.Contains(t, response.Body.String(), requestpolicy.CandidateUnavailableReason)
			}
		})
	}
}

// strictParameterHTTPProvider creates one mapped upstream endpoint and
// attaches optional versioned test evidence. Nil support leaves the exact
// route without a declared verdict while keeping it otherwise authorized.
func strictParameterHTTPProvider(id, group int64, priority int, platform, url, alias, model string, inbound, outbound protocol.ProtocolID, support *bool) gatewayprovider.ExecutionProvider {
	extra := map[string]any{}
	if outbound == protocol.ProtocolOpenAIChatCompletions {
		extra[provider.ExtraKeyTextRouteMode] = string(provider.TextRouteModeForceChatCompletions)
	}
	if support != nil {
		extra[provider.RequestParameterEvidenceExtraKey] = []map[string]any{{
			"model": model, "inbound": string(inbound), "outbound": string(outbound),
			"source": "http-receipt-fixture", "version": "1",
			"fields": map[string]bool{"temperature": *support},
		}}
	}
	return *gatewayprovider.NewExecutionProvider(&provider.Record{
		ID: id, Name: model, Platform: platform, Type: provider.ProviderTypeAPIKey, Status: provider.StatusActive,
		Schedulable: true, Priority: priority, Concurrency: 1, GroupIDs: []int64{group}, Extra: extra,
		Credentials: map[string]any{"api_key": "fixture", "base_url": url, "model_mapping": map[string]any{alias: model}},
	})
}
