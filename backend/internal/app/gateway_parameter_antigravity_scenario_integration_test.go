//go:build integration

package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/apikey/testkit"
	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/gateway/execution"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewaypg "github.com/TokenFlux/TokenRouter/internal/gateway/postgres"
	"github.com/TokenFlux/TokenRouter/internal/gateway/promptpolicy"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/routing/capability"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/upstream"
)

// gateway_parameter_antigravity_scenario_integration_test.go drives the
// registered Antigravity Messages entry with real key authentication and
// durable cohort storage. A forwarding fixture sends HTTP to a loopback
// provider so the test can observe dispatch without provider credentials.

// antigravityParameterExecutor connects the routed Messages handler to fixture forwarding.
type antigravityParameterExecutor func(context.Context, execution.Request, upstream.OutputSink) (execution.ExecutionResult, error)

// antigravityParameterRecorder counts calls around the production cohort repository.
type antigravityParameterRecorder struct {
	responsecompat.CohortRecorder             // production database recorder
	finished                      chan string // completed recorder call identities
}

// TestGatewayParameterPolicyAntigravityAlias checks that a key model redirect
// preserves the original policy match in the dedicated production route.
// Observe sends once and completes; strict rejects the same original schema
// locally. Both return paths finalize their database row exactly once.
func TestGatewayParameterPolicyAntigravityAlias(t *testing.T) {
	db := qualityTestDatabase(t)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	policy := requestpolicy.New(provideSettingsStore(client))
	finishes := make(chan string, 8)
	cohorts := antigravityParameterRecorder{CohortRecorder: gatewaypg.NewCompatibilityCohorts(db), finished: finishes}
	const path = "/antigravity/v1/messages"
	const originalModel, mappedModel = "fixture-antigravity-alias", "claude-sonnet-4-5"
	const schema = `,"temperature":0,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"x":{"$ref":"#/$defs/missing"}}}}]`
	const reply = `{"id":"msg_antigravity","type":"message","role":"assistant","model":"` + mappedModel + `","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`
	var observations sync.Mutex
	var receipts []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		observations.Lock()
		receipts = append(receipts, string(body))
		observations.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(provider.Close)
	var intents []requestpolicy.Intent
	var platforms []string
	executor := antigravityParameterExecutor(func(ctx context.Context, in execution.Request, sink upstream.OutputSink) (result execution.ExecutionResult, err error) {
		intent, _ := requestpolicy.FromContext(ctx)
		observations.Lock()
		intents = append(intents, intent)
		platforms = append(platforms, in.Text.Platform)
		observations.Unlock()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, bytes.NewReader(in.Body))
		if err != nil {
			return result, err
		}
		up, err := provider.Client().Do(request)
		if err != nil {
			return result, err
		}
		// Consume and close the recorded provider response before passing its
		// complete payload through the gateway's real synchronous output sink.
		body, err := io.ReadAll(up.Body)
		if err := errors.Join(err, up.Body.Close()); err != nil {
			return result, err
		}
		if err := sink.Begin(upstream.OutputHead{Status: up.StatusCode, Header: up.Header}); err != nil {
			return result, err
		}
		err = sink.Emit(upstream.OutputEvent{Data: body, Semantic: true, Terminal: true})
		result.Attempt.Served = err == nil
		return result, err
	})
	cfg := &config.Config{}
	cfg.Gateway.MaxRequestBodyBytes = 64 << 10
	cfg.Gateway.TextMaxRequestBodyBytes = cfg.Gateway.MaxRequestBodyBytes
	group := &routing.Group{
		ID: parameterFixtureGroup, Status: "active", Hydrated: true,
		AllowedProtocols: []protocol.ProtocolID{protocol.ProtocolAnthropicMessages},
	}
	user := &identity.User{ID: parameterFixtureUser, Status: "active", Balance: 100}
	key := &apikey.APIKey{
		ID: parameterFixtureKey, Key: parameterFixtureToken, Status: "active", BillingMode: "balance",
		UserID: user.ID, User: user, GroupID: &group.ID, Group: group, ModelMapping: map[string]string{originalModel: mappedModel},
	}
	keys := testkit.NewService(&openCodeHTTPKeys{key: key}, nil, nil, nil, nil, nil, cfg)
	t.Cleanup(keys.Stop)
	eligibility := newBillingEligibilityFixture(cfg)
	t.Cleanup(eligibility.Stop)
	bindings := gatewayhttp.MessagesBindings{
		PlanRoute: gatewayprovider.NewRoutePlanner(nil).PlanKey, ClientVersions: func(context.Context) (string, string) { return "", "" },
		Funding:       newFundingAdmissionFixture(eligibility, cfg),
		CachedSession: func(context.Context, *int64, string) (int64, error) { return 0, nil }, ObserveCompatibility: func(*zap.Logger) {},
	}
	messages := gatewayhttp.NewBoundMessagesHandler(gatewayhttp.MessagesHTTPOptions{}, bindings, (*promptpolicy.Service)(nil),
		provideOpenAIHTTPResources(scheduler.NewConcurrencyService(nil), cfg).Concurrency, executor)
	text := gatewayhttp.NewOpenAITextHandler(gatewayhttp.OpenAITextOptions{ParameterPolicy: policy, CompatibilityCohorts: cohorts}, nil, nil, nil)
	router := gin.New()
	RegisterGatewayRoutes(router, &routeTestHandlers{TextEnabled: true, OpenAIEnabled: true, MessagesHTTP: messages, OpenAITextHTTP: text}, provideAPIKeyAuth(keys, nil, cfg), keys, nil, nil, nil, cfg)
	gateway := httptest.NewServer(router)
	t.Cleanup(gateway.Close)
	httpClient := gateway.Client()
	httpClient.Timeout = parameterFixtureTimeout

	var requestIDs []string
	payload := parameterPayload(gatewayhttp.EndpointMessages, originalModel, false, schema)
	for i, mode := range []requestpolicy.Mode{requestpolicy.ModeObserve, requestpolicy.ModeStrict} {
		status, err := policy.Update(t.Context(), requestpolicy.Config{Revision: int64(i), Mode: mode, Rules: []requestpolicy.Rule{{
			Entry: requestpolicy.Entry{EntryGroupID: group.ID, Endpoint: requestpolicy.EndpointMessages, RequestedModel: originalModel},
			Mode:  requestpolicy.ModeStrict, StrictPercent: 100,
		}}})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gateway.URL+path, strings.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+parameterFixtureToken)
		req.Header.Set("Content-Type", "application/json")
		res, err := httpClient.Do(req)
		require.NoError(t, err)
		body, readErr := io.ReadAll(res.Body)
		require.NoError(t, res.Body.Close())
		require.NoError(t, readErr)
		id := res.Header.Get("X-TokenRouter-Request-ID")
		require.NotEmpty(t, id)
		requestIDs = append(requestIDs, id)
		if mode == requestpolicy.ModeObserve {
			require.Equal(t, http.StatusOK, res.StatusCode, string(body))
			require.Equal(t, originalModel, gjson.GetBytes(body, "model").String())
		} else {
			require.Equal(t, http.StatusBadRequest, res.StatusCode, string(body))
			require.Equal(t, "error", gjson.GetBytes(body, "type").String())
			require.Contains(t, gjson.GetBytes(body, "error.message").String(), "tools[0].input_schema")
		}
		t.Logf("%s %s returned HTTP %d at revision %d", path, mode, res.StatusCode, status.Settings.Revision)
	}
	// Join both listeners before reading dispatch observations and deferred
	// finalization records because client EOF can precede handler cleanup.
	gateway.Close()
	provider.Close()
	require.Len(t, receipts, 1)
	require.Equal(t, mappedModel, gjson.Get(receipts[0], "model").String())
	require.Equal(t, "0", gjson.Get(receipts[0], "temperature").Raw)
	require.Equal(t, "#/$defs/missing", gjson.Get(receipts[0], "tools.0.input_schema.properties.x.$ref").String())
	require.Equal(t, []string{capability.PlatformAntigravity}, platforms)
	require.Len(t, intents, 1)
	require.Equal(t, requestpolicy.ModeObserve, intents[0].Mode)
	require.Equal(t, payload, intents[0].RawJSON())
	close(finishes)
	var finalized []string
	for id := range finishes {
		finalized = append(finalized, id)
	}
	require.ElementsMatch(t, requestIDs, finalized)
	assertParameterCohort(t, db, requestIDs[0], 1, "observe", "completed", "", requestpolicy.EndpointMessages, originalModel)
	assertParameterCohort(t, db, requestIDs[1], 2, "strict", "parameter_400", "entry", requestpolicy.EndpointMessages, originalModel)
	t.Logf("observe sent 1 with %s, completed once; strict sent 0, parameter_400/entry once; both matched %s", mappedModel, originalModel)
}

// Execute forwards the prepared request through the supplied loopback callback.
func (f antigravityParameterExecutor) Execute(ctx context.Context, in execution.Request, sink upstream.OutputSink) (execution.ExecutionResult, error) {
	return f(ctx, in, sink)
}

// Finish records each repository call, including duplicate attempts the SQL
// primary key would otherwise hide. The test drains the channel after the HTTP
// listener has finished all handler cleanup.
func (r antigravityParameterRecorder) Finish(ctx context.Context, entry responsecompat.Cohort, result responsecompat.CohortResult) error {
	r.finished <- entry.RequestID
	return r.CohortRecorder.Finish(ctx, entry, result)
}
