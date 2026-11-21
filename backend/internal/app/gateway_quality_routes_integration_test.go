//go:build integration

package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
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
	routingtestkit "github.com/TokenFlux/TokenRouter/internal/routing/testkit"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
)

// gateway_quality_routes_integration_test.go drives real gateway requests while
// a scheduled probe changes a provider's eligibility. Loopback servers record
// the final model independently of scheduler observations. Each load worker
// owns its billing journals and reads the same PostgreSQL database.

// qualityHTTPCase describes one public entry and the expected wire destination.
type qualityHTTPCase struct {
	path, model, actual       string // inbound path, inbound name and expected wire model
	count, winner             int    // pool size and one-based rank of the successful provider
	native                    bool   // body-signaled compaction on the ordinary Responses path
	cross, denied, off, retry bool   // authorized fallback, denied destination, bypass and HTTP 502 controls
}

// verifyQualityWSTurn commits a failing result between two real frames and
// checks the close code and journals after handler exit. The pooled upstream
// connection can outlive the client until fixture cleanup closes its owner.
func verifyQualityWSTurn(t *testing.T, id int64, mode string, read gatewayprovider.PlanModelRestrictionReader, tick func() int64) int64 {
	t.Helper()
	var frames atomic.Int64
	done := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == "http_bridge" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			frames.Add(1)
			if r.URL.Path != "/v1/responses" || gjson.GetBytes(body, "model").String() != "gpt-5.4" {
				t.Errorf("unexpected bridge target %s %s", r.URL.Path, gjson.GetBytes(body, "model"))
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			mixedUpstreamResponse(w, r, false)
			return
		}
		defer close(done)
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for {
			_, body, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			frames.Add(1)
			if actual := gjson.GetBytes(body, "model").String(); actual != "gpt-5.4" {
				t.Errorf("unexpected wire model %q", actual)
			}
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_quality_first","model":"gpt-5.4","usage":{"input_tokens":10,"output_tokens":3}}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	logs := &gatewaytestkit.UsageLogStore{Inserted: true}
	funds := &gatewaytestkit.SettlementStore{}
	h := newConfiguredWSPassthroughHarness(t, upstream.URL, func(f *gatewayHTTPFixtureInput, p *gatewayprovider.ExecutionProvider, group *routing.Group) {
		p.Record.ID = id
		p.Record.Extra[provider.ResponsesWSConnectionModeKey] = mode
		p.Record.GroupIDs = []int64{group.ID}
		if mode == "http_bridge" {
			p.Record.Credentials["upstream_protocols"] = []string{string(protocol.ProtocolOpenAIResponses)}
			p.Record.Extra["openai_apikey_responses_websockets_v2_enabled"] = false
			group.ProtocolFallbacks = map[protocol.ProtocolID][]protocol.ProtocolID{protocol.ProtocolResponsesWebSocket: {protocol.ProtocolOpenAIResponses}}
			f.Source.Requests.Transport = &mixedHTTPTransport{}
		}
		t.Cleanup(f.Source.WebSockets.Connections.Close)
		f.Moderator = nil
		f.Config.Gateway.OpenAI.PlanModelRestrictions = true
		f.PlanModelRestrictions = read
		recorder := gatewaytestkit.NewRecording(logs, funds, nil, false)
		recorder.Options.DefaultMultiplier = 1
		f.Source.Recorder = recorder.Core(nil, true)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	send := func(payload string) {
		t.Helper()
		require.NoError(t, h.clientConn.Write(ctx, coderws.MessageText, []byte(payload)))
	}
	send(`{"type":"response.create","model":"gpt-5.4","input":"first"}`)
	var responseID string
	for responseID == "" {
		_, body, err := h.clientConn.Read(ctx)
		require.NoError(t, err)
		if gjson.GetBytes(body, "type").String() == "response.completed" {
			responseID = gjson.GetBytes(body, "response.id").String()
		}
	}
	result := tick()
	send(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","input":"second","previous_response_id":%q}`, responseID))
	_, _, err := h.clientConn.Read(ctx)
	require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err), "%v", err)
	require.Contains(t, err.Error(), "selected provider model is unavailable")
	channels := []<-chan struct{}{h.handlerDone}
	if mode == provider.ResponsesWSPerSession {
		channels = append(channels, done)
	}
	for _, ch := range channels {
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.EqualValues(t, 1, frames.Load())
	require.Equal(t, 1, logs.Calls)
	require.Equal(t, 1, funds.Calls)
	require.Equal(t, id, logs.LastLog.ProviderID)
	require.Equal(t, 10, logs.LastLog.InputTokens)
	require.Equal(t, 3, logs.LastLog.OutputTokens)
	t.Logf("WS mode=%s provider=%d model=gpt-5.4 first=completed result=%d second=%v wire_frames=1 usage=1 settlements=1", mode, id, result, err)
	return result
}

// verifyQualityRoutes checks a late Compact verdict, authorized conversions and
// the disabled gate. After recovery it measures committed reads and round-trip
// latency under independent client load, including a provider switch on HTTP 502.
func verifyQualityRoutes(t *testing.T, ids []int64, read gatewayprovider.PlanModelRestrictionReader, restrict, recoverPlan func()) {
	t.Helper()
	compact := qualityHTTPCase{path: "/v1/responses/compact", model: "compact-x", actual: "gpt-5.4", count: 2, winner: 2}
	var primaryReads atomic.Int32
	late := func(ctx context.Context, id int64, actual string, at time.Time) ([]provider.ActivePlanModelRestriction, error) {
		if id == ids[0] && actual == "gpt-5.4" && primaryReads.Add(1) == 2 {
			restrict()
			at = time.Now()
		}
		return read(ctx, id, actual, at)
	}
	newQualityHTTPClient(t, ids, compact, late)()
	require.GreaterOrEqual(t, primaryReads.Load(), int32(2))
	newQualityHTTPClient(t, ids, compact, read)()
	remap := compact
	remap.model, remap.actual, remap.winner = "compact-y", "gpt-5.2", 1
	newQualityHTTPClient(t, ids, remap, read)()
	bypass := compact
	bypass.off, bypass.winner = true, 1
	newQualityHTTPClient(t, ids, bypass, func(context.Context, int64, string, time.Time) ([]provider.ActivePlanModelRestriction, error) {
		t.Error("disabled gate queried")
		return nil, nil
	})()
	cross := qualityHTTPCase{path: "/v1/chat/completions", model: "public-x", actual: "gpt-5.4", count: 2, winner: 2, cross: true}
	newQualityHTTPClient(t, ids, cross, read)()
	messages := cross
	messages.path = "/v1/messages"
	newQualityHTTPClient(t, ids, messages, read)()
	native := qualityHTTPCase{native: true, path: "/v1/responses", model: "compact-y", actual: "gpt-5.4", count: 2, winner: 2}
	newQualityHTTPClient(t, ids, native, read)()
	cross.denied = true
	newQualityHTTPClient(t, ids, cross, read)()
	recoverPlan()
	for _, m := range []struct{ candidates, workers, retries int }{{1, 1, 0}, {2, 1, 0}, {4, 4, 0}, {4, 4, 1}} {
		var reads atomic.Int64
		counted := func(ctx context.Context, id int64, model string, at time.Time) ([]provider.ActivePlanModelRestriction, error) {
			reads.Add(1)
			return read(ctx, id, model, at)
		}
		const n = 24
		durations := make([]time.Duration, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for worker := 0; worker < m.workers; worker++ {
			c := qualityHTTPCase{path: "/v1/responses", model: "client-x", actual: "gpt-5.4", count: m.candidates, winner: 1 + m.retries, retry: m.retries > 0}
			call := newQualityHTTPClient(t, ids, c, counted)
			wg.Go(func() {
				<-start
				for i := worker; i < n; i += m.workers {
					durations[i] = call()
				}
			})
		}
		close(start)
		wg.Wait()
		slices.Sort(durations)
		require.EqualValues(t, n*(m.candidates+1+m.retries), reads.Load())
		t.Logf("n=%d concurrency=%d candidates=%d retries=%d SQL/request=%.2f HTTP p50=%s p95=%s", n, m.workers, m.candidates, m.retries, float64(reads.Load())/n, durations[11], durations[22])
	}
}

// newQualityHTTPClient owns one worker's HTTP listener and synchronous billing
// journals. Requests from different workers share only the durable verdict source.
func newQualityHTTPClient(t *testing.T, ids []int64, tc qualityHTTPCase, read gatewayprovider.PlanModelRestrictionReader) func() time.Duration {
	t.Helper()
	cfg := &config.Config{}
	cfg.Billing.FallbackRateMultiplier = 1
	cfg.Gateway.OpenAI.PlanModelRestrictions = !tc.off
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.Failover.MaxProviderSwitches = 1
	inbound := protocol.ProtocolOpenAIResponses
	if tc.cross {
		inbound = protocol.ProtocolOpenAIChatCompletions
	}
	if tc.path == "/v1/messages" {
		inbound = protocol.ProtocolAnthropicMessages
	}
	group := &routing.Group{
		ID: 42, Name: "destination", Status: "active", Hydrated: true, RateMultiplier: 1,
		AllowedProtocols:  []protocol.ProtocolID{inbound, protocol.ProtocolResponsesCompact},
		ProtocolFallbacks: map[protocol.ProtocolID][]protocol.ProtocolID{inbound: {protocol.ProtocolOpenAIResponses}},
	}
	if tc.denied {
		group.IsExclusive = true
	}
	origin := group
	var pricing *routing.PricingConfigService
	if tc.cross {
		origin = &routing.Group{ID: 41, Name: "source", Status: "active", Hydrated: true, ClaudeCodeOnly: true, FallbackGroupID: &group.ID, AllowedProtocols: []protocol.ProtocolID{inbound}}
		pricing = newGatewayExecutionPricingConfigServiceForTest(group.ID, "openai", routingtestkit.Configuration{ID: 88, Name: "destination", Status: "active", ModelMapping: map[string]string{"public-x": "client-x"}})
	}
	pool := &mixedHTTPProviders{}
	seen := make(chan string, 8)
	wirePath := "/v1/responses"
	if strings.HasSuffix(tc.path, "/compact") {
		wirePath = tc.path
	}
	for rank := 1; rank <= tc.count; rank++ {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "bad request", 400)
				return
			}
			seen <- fmt.Sprintf("P%d %s %s", rank, r.URL.Path, gjson.GetBytes(body, "model").String())
			if tc.retry && rank == 1 {
				http.Error(w, "temporarily unavailable", 502)
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			mixedUpstreamResponse(w, r, false)
		}))
		t.Cleanup(upstream.Close)
		id := ids[rank-1]
		record := &provider.Record{
			ID: id, Name: fmt.Sprintf("P%d", rank), Platform: provider.PlatformOpenAI, Type: provider.ProviderTypeAPIKey,
			Status: "active", Schedulable: true, Priority: rank, Concurrency: 1, GroupIDs: []int64{group.ID},
			Extra: map[string]any{"openai_compact_mode": "force_support", "openai_native_compaction_v2_mode": "force_support"},
			Credentials: map[string]any{
				"api_key": "fixture", "base_url": upstream.URL,
				"model_mapping":         map[string]any{"client-x": "gpt-5.4", "compact-x": "gpt-5.2", "compact-y": "gpt-5.4"},
				"compact_model_mapping": map[string]any{"compact-x": "gpt-5.4", "compact-y": "gpt-5.2"},
			},
		}
		if tc.cross {
			record.Credentials["upstream_protocols"] = []string{string(protocol.ProtocolOpenAIResponses)}
		}
		pool.values = append(pool.values, *gatewayprovider.NewExecutionProvider(record))
	}
	transport := &mixedHTTPTransport{}
	source, choices, credentials := newOpenAIExecutionAndSelectionFixture(pool, nil, cfg, nil, nil, nil, transport, nil, nil, nil, nil, nil, pricing, nil, nil, nil, nil, nil, nil)
	logs := &gatewaytestkit.UsageLogStore{Inserted: true}
	funds := &gatewaytestkit.SettlementStore{}
	recording := gatewaytestkit.NewRecording(logs, funds, nil, false)
	recording.Options.DefaultMultiplier = 1
	source.Recorder = recording.Core(nil, true)
	user := &identity.User{ID: 7, Status: "active", Balance: 100}
	key := &apikey.APIKey{ID: 8, Key: "fixture-client", Status: "active", UserID: user.ID, User: user, GroupID: &origin.ID, Group: origin, BillingMode: "balance"}
	groups := fallbackGroupRepository{groups: map[int64]*routing.Group{origin.ID: origin, group.ID: group}}
	keys := testkit.NewService(&openCodeHTTPKeys{key: key}, fallbackUserRepository{user: user}, groups, nil, nil, nil, cfg)
	t.Cleanup(keys.Stop)
	fundsCheck := newBillingEligibilityFixture(cfg)
	funding := newFundingAdmissionFixture(fundsCheck, cfg)
	t.Cleanup(fundsCheck.Stop)
	endpoints := newGatewayHTTPEndpoints(gatewayHTTPFixtureInput{PlanModelRestrictions: read, Source: source, Choices: choices, Credentials: credentials, Keys: keys, Config: cfg, Funding: funding, Concurrency: provideOpenAIHTTPResources(scheduler.NewConcurrencyService(nil), cfg).Concurrency, MaxSwitches: 1})
	auth := provideAPIKeyAuth(keys, nil, cfg)
	mw := legacyRouteMiddleware(auth, keys, nil, nil, nil, cfg)
	mw.ClientGroupFallback = provideClientGroupFallbackResolver(keys, funding, nil, nil)
	guards := gatewayhttp.NewRouteGuards(mw)
	router := gin.New()
	endpoint := endpoints.Responses
	if tc.cross {
		endpoint = endpoints.ChatCompletions
	}
	if tc.path == "/v1/messages" {
		endpoint = endpoints.Messages
	}
	router.POST(tc.path, gin.HandlerFunc(auth), guards.RequireGroupClientProtocol(inbound, gatewayhttp.GroupClientProtocolErrorOpenAI), endpoint)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	server.Client().Timeout = 15 * time.Second
	return func() time.Duration {
		t.Helper()
		before := logs.Calls
		payload := fmt.Sprintf(`{"model":%q,"input":"hello","stream":false}`, tc.model)
		if tc.cross {
			payload = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"stream":false}`, tc.model)
		}
		if tc.native {
			payload = fmt.Sprintf(`{"model":%q,"stream":true,"input":[{"type":"compaction_trigger"}]}`, tc.model)
		}
		req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+tc.path, strings.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer fixture-client")
		req.Header.Set("Content-Type", "application/json")
		start := time.Now()
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		elapsed := time.Since(start)
		require.NoError(t, err)
		if tc.denied {
			require.Equal(t, 403, resp.StatusCode, string(data))
			require.Empty(t, seen)
			require.Equal(t, before, logs.Calls)
			require.Equal(t, before, funds.Calls)
			t.Logf("denied fallback HTTP=403 attempts=0 usage=0 settlements=0")
			return elapsed
		}
		require.Equal(t, 200, resp.StatusCode, string(data))
		if tc.retry {
			require.Len(t, seen, 2)
			require.Equal(t, "P1 "+wirePath+" "+tc.actual, <-seen)
		}
		require.Len(t, seen, 1)
		exchange := <-seen
		require.Equal(t, fmt.Sprintf("P%d %s %s", tc.winner, wirePath, tc.actual), exchange)
		require.Equal(t, before+1, logs.Calls)
		require.Equal(t, logs.Calls, funds.Calls)
		require.Equal(t, ids[tc.winner-1], logs.LastLog.ProviderID)
		require.Equal(t, group.ID, *logs.LastLog.GroupID)
		if before == 0 {
			t.Logf("gate=%v %s group=%d -> %d model=%s received=%s usage=1 settlements=1", !tc.off, tc.path, origin.ID, group.ID, tc.model, exchange)
		}
		return elapsed
	}
}
