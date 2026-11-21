//go:build integration

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/apikey/testkit"
	"github.com/TokenFlux/TokenRouter/internal/app/lifecycle"
	"github.com/TokenFlux/TokenRouter/internal/config"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/provider/selection"
	gatewaytestkit "github.com/TokenFlux/TokenRouter/internal/gateway/testkit"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	postgresinfra "github.com/TokenFlux/TokenRouter/internal/infra/postgres"
	catalogtest "github.com/TokenFlux/TokenRouter/internal/modelcatalog/testkit"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	providerhttp "github.com/TokenFlux/TokenRouter/internal/provider/httpapi"
	providerpg "github.com/TokenFlux/TokenRouter/internal/provider/postgres"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/routing/accessview"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/scheduler/policy"
	"github.com/TokenFlux/TokenRouter/internal/testutil/postgrescontainer"
	"github.com/TokenFlux/TokenRouter/migrations"
)

// A controlled clock tick drives the production runner through a loopback probe.
// Independent loopback clients then observe which provider serves each model.
type qualityPreviewPool struct {
	group               *routing.Group // selected diagnostic group
	*mixedHTTPProviders                // candidate records shared with the HTTP fixture
}

// qualityTestSchedule captures one callback and dispatches a due round on demand.
type qualityTestSchedule struct{ tick func() }

// Start records the synchronous tick so the test can run a due plan to completion.
func (s *qualityTestSchedule) Start(_ context.Context, tick func()) error { s.tick = tick; return nil }

// Stop owns no asynchronous resources in the controlled schedule.
func (*qualityTestSchedule) Stop(context.Context) error { return nil }

// TestScheduledQualityResponses checks a saved probe, its owner, and the actual
// receiving server on both sides of a restriction and recovery.
func TestScheduledQualityResponses(t *testing.T) {
	ctx := t.Context()
	db := qualityTestDatabase(t)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	store := provideProviderStore(client, db, nil)
	group := &routing.Group{ID: 42, Hydrated: true, Name: "G", Status: "active", RateMultiplier: 1, AllowedProtocols: []protocol.ProtocolID{protocol.ProtocolOpenAIResponses}}
	cfg := &config.Config{}
	cfg.Billing.FallbackRateMultiplier = 1
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Security.URLAllowlist.AllowPrivateHosts = true
	cfg.Gateway.Failover.MaxProviderSwitches = 1
	providers := &mixedHTTPProviders{}
	var answer atomic.Value
	answer.Store("[0,0,0]")
	probeWire := make(chan string, 4)
	received := make(chan string, 4)
	var ids []int64
	for rank := 1; rank <= 2; rank++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "invalid request", 400)
				return
			}
			actual := gjson.GetBytes(body, "model").String()
			if r.Header.Get("X-Fixture-Probe") == "1" {
				probeWire <- fmt.Sprintf("%s %s %s", r.URL.Path, actual, gjson.GetBytes(body, "input.0.content.0.text").String())
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\ndata: {\"type\":\"response.completed\"}\n\n", answer.Load().(string))
				return
			}
			received <- fmt.Sprintf("P%d %s %s", rank, r.URL.Path, actual)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			mixedUpstreamResponse(w, r, false)
		}))
		t.Cleanup(srv.Close)
		row, err := client.Provider.Create().SetName(fmt.Sprintf("P%d", rank)).SetPlatform("openai").SetType("apikey").SetCredentials(map[string]any{"api_key": "fixture-key", "base_url": srv.URL, "model_mapping": map[string]any{"client-x": "gpt-5.4", "gpt-5.2": "gpt-5.2"}}).Save(ctx)
		require.NoError(t, err)
		record, err := store.GetByID(ctx, row.ID)
		require.NoError(t, err)
		record.Priority = rank
		record.Concurrency = 1
		record.GroupIDs = []int64{group.ID}
		record.Schedulable = true
		providers.values = append(providers.values, *gatewayprovider.NewExecutionProvider(record))
		ids = append(ids, row.ID)
	}
	plans := providerpg.NewScheduledTestPlanRepository(db)
	scheduled := provideScheduledTests(plans, providerpg.NewScheduledTestResultRepository(db), store, db)
	admin := gin.New()
	admin.POST("/plans", providerhttp.NewScheduledTestHandler(scheduled).Create)
	body := fmt.Sprintf(`{"provider_id":%d,"model_id":"client-x","cron_expression":"* * * * *","quality_enabled":true}`, ids[0])
	create := httptest.NewRequest("POST", "/plans", strings.NewReader(body))
	create.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	admin.ServeHTTP(out, create)
	require.Equal(t, 200, out.Code, out.Body.String())
	var plan provider.ScheduledTestPlan
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &plan))
	manager := lifecycle.New()
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	probes := &mixedHTTPTransport{failure: func(req *http.Request, _ int64) error { req.Header.Set("X-Fixture-Probe", "1"); return nil }}
	attributes := &routing.ModelAttributeService{Catalog: routing.ModelAttributeCatalog{View: catalogtest.OfflineView}}
	core := provideProviderTests(store, nil, nil, nil, nil, probes, cfg, nil, nil, nil, nil, manager, nil, attributes, nil)
	schedule := &qualityTestSchedule{}
	runner := provider.NewScheduledTestRunnerService(plans, scheduled, core, provider.ScheduledRunnerOptions{Schedule: schedule, Now: time.Now, NextRun: func(_ string, now time.Time) (time.Time, error) { return now.Add(time.Hour), nil }, Observe: t.Logf})
	require.NoError(t, runner.StartContext(ctx))
	t.Cleanup(runner.Stop)
	tick := func() int64 {
		t.Helper()
		_, err := db.ExecContext(ctx, "UPDATE scheduled_test_plans SET next_run_at=now() WHERE id=$1", plan.ID)
		require.NoError(t, err)
		start := time.Now()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		initial := mem.HeapAlloc
		sampled := make(chan uint64, 1)
		done := make(chan struct{})
		go func() {
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			peak := initial
			for {
				runtime.ReadMemStats(&mem)
				peak = max(peak, mem.HeapAlloc)
				select {
				case <-done:
					sampled <- peak
					return
				case <-ticker.C:
				}
			}
		}()
		schedule.tick()
		close(done)
		peak := <-sampled
		t.Logf("probe tick=%s process heap sampled initial=%d peak=%d bytes", time.Since(start), initial, peak)
		history, err := scheduled.ListResults(ctx, plan.ID, 1)
		require.NoError(t, err)
		require.Len(t, history, 1)
		require.Equal(t, "success", history[0].Status, history[0].ErrorMessage)
		require.Len(t, probeWire, 1)
		wire := <-probeWire
		require.Contains(t, wire, "/v1/responses gpt-5.4 Return only a JSON array")
		t.Logf("plan=%d result=%d probe=%s response=%s tick=%s", plan.ID, history[0].ID, wire, history[0].ResponseText, time.Since(start))
		return history[0].ID
	}
	var reads int
	makeGateway := func(enabled bool) func(string, int, string) time.Duration {
		gatewayCfg := *cfg
		cfg := &gatewayCfg
		cfg.Gateway.OpenAI.PlanModelRestrictions = enabled
		var reader gatewayprovider.PlanModelRestrictionReader
		if enabled {
			reader = func(ctx context.Context, id int64, model string, at time.Time) ([]provider.ActivePlanModelRestriction, error) {
				reads++
				return store.ActivePlanModelRestrictions(ctx, id, model, at)
			}
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
			PlanModelRestrictions: reader,
			Source:                source, Credentials: credentials, Choices: choices, Config: cfg, Keys: keys,
			Funding: newFundingAdmissionFixture(eligibility, cfg), Concurrency: resources.Concurrency, MaxSwitches: 1,
		})
		auth := provideAPIKeyAuth(keys, nil, cfg)
		guards := gatewayhttp.NewRouteGuards(legacyRouteMiddleware(auth, keys, nil, nil, nil, cfg))
		router := gin.New()
		const path = "/v1/responses"
		router.POST(path, gin.HandlerFunc(auth), guards.RequireGroupClientProtocol(
			protocol.ProtocolOpenAIResponses, gatewayhttp.GroupClientProtocolErrorOpenAI), endpoints.Responses)

		server := httptest.NewServer(router)
		t.Cleanup(server.Close)
		return func(model string, want int, actual string) time.Duration {
			t.Helper()
			before := logs.Calls
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","stream":false}`, model)))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer fixture-client")
			req.Header.Set("Content-Type", "application/json")
			start := time.Now()
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			elapsed := time.Since(start)
			require.Equal(t, 200, resp.StatusCode, string(data))
			require.Len(t, received, 1)
			observed := <-received
			require.Equal(t, fmt.Sprintf("P%d /v1/responses %s", want, actual), observed)
			require.Equal(t, before+1, logs.Calls)
			require.Equal(t, logs.Calls, funds.Calls)
			calls := transport.calls()
			require.Len(t, calls, logs.Calls)
			require.Equal(t, ids[want-1], calls[len(calls)-1])
			require.Equal(t, ids[want-1], logs.LastLog.ProviderID)
			if before < 5 {
				t.Logf("flag=%v model=%s received=%s attempts=%v usage=%d settlements=%d", enabled, model, observed, calls, logs.Calls, funds.Calls)
			}
			return elapsed
		}
	}
	on := makeGateway(true)
	on("client-x", 1, "gpt-5.4")
	bad := verifyQualityWSTurn(t, ids[0], provider.ResponsesWSPerSession, store.ActivePlanModelRestrictions, tick)
	on("client-x", 2, "gpt-5.4")
	on("gpt-5.2", 1, "gpt-5.2")
	owners, err := store.ActivePlanModelRestrictions(ctx, ids[0], "gpt-5.4", time.Now())
	require.NoError(t, err)
	require.Len(t, owners, 1)
	require.Equal(t, bad, owners[0].ResultID)
	require.Equal(t, plan.ID, owners[0].PlanID)
	previewGroup := *group
	previewGroup.SchedulerType = routing.GroupSchedulerTypeAdvanced
	previewPool := qualityPreviewPool{group: &previewGroup, mixedHTTPProviders: &mixedHTTPProviders{values: slices.Clone(providers.values)}}
	for i := range previewPool.values {
		previewPool.values[i].Record.ProviderGroups = []provider.GroupMembership{{GroupID: group.ID, Group: (*accessview.GroupConfig)(&previewGroup)}}
	}
	diagnostic := selection.NewDiagnostics(previewPool, selection.Shared{PlanModelRestrictions: store.ActivePlanModelRestrictions}, nil, nil)
	previewRequest := policy.AdvancedSchedulerScoreDiagnosticRequest{GroupID: group.ID, RequestedModel: "client-x", HTTPResponses: true}
	detail, err := diagnostic.GetDetail(ctx, ids[0], previewRequest)
	require.NoError(t, err)
	require.Contains(t, detail.Detail.HardFilterReasons, "plan_model_restricted")
	check := detail.Detail.PlanModelCheck
	require.Equal(t, "gpt-5.4", check.ActualModel)
	require.Len(t, check.Owners, 1)
	require.Equal(t, bad, check.Owners[0].ResultID)
	require.Equal(t, plan.ID, check.Owners[0].PlanID)
	require.Equal(t, "arithmetic-array-v1: incorrect_answer", check.Owners[0].Reason)
	t.Logf("preview result=%d actual=%s reason=%s", bad, check.ActualModel, check.Owners[0].Reason)
	off := makeGateway(false)
	off("client-x", 1, "gpt-5.4")
	answer.Store("[15,5,21]")
	good := tick()
	require.Greater(t, good, bad)
	on("client-x", 1, "gpt-5.4")
	owners, err = store.ActivePlanModelRestrictions(ctx, ids[0], "gpt-5.4", time.Now())
	require.NoError(t, err)
	require.Empty(t, owners)
	detail, err = diagnostic.GetDetail(ctx, ids[0], previewRequest)
	require.NoError(t, err)
	require.Empty(t, detail.Detail.PlanModelCheck.Owners)
	require.NotContains(t, detail.Detail.HardFilterReasons, "plan_model_restricted")
	var restricted bool
	require.NoError(t, db.QueryRowContext(ctx, "SELECT restricted FROM provider_plan_model_verdicts WHERE result_id=$1", good).Scan(&restricted))
	require.False(t, restricted)
	for _, sample := range []struct {
		name string
		send func(string, int, string) time.Duration
	}{{"off", off}, {"on", on}} {
		const n = 24
		before := reads
		var latencies []time.Duration
		for i := 0; i < n; i++ {
			latencies = append(latencies, sample.send("client-x", 1, "gpt-5.4"))
		}
		if sample.name == "on" {
			require.Equal(t, 3*n, reads-before)
		} else {
			require.Equal(t, before, reads)
		}
		slices.Sort(latencies)
		t.Logf("flag=%s n=%d concurrency=1 candidates=2 reads/request=%.2f latency p50=%s p95=%s", sample.name, n, float64(reads-before)/n, latencies[11], latencies[22])
	}
	answer.Store("[0,0,0]")
	for _, mode := range []string{provider.ResponsesWSPooled, "http_bridge"} {
		verifyQualityWSTurn(t, ids[0], mode, store.ActivePlanModelRestrictions, tick)
		answer.Store("[15,5,21]")
		tick()
		answer.Store("[0,0,0]")
	}
	for rank := 3; rank <= 4; rank++ {
		row, err := client.Provider.Create().SetName(fmt.Sprintf("P%d", rank)).SetPlatform("openai").SetType("apikey").Save(ctx)
		require.NoError(t, err)
		ids = append(ids, row.ID)
	}
	verifyQualityRoutes(t, ids, store.ActivePlanModelRestrictions, func() { tick() }, func() { answer.Store("[15,5,21]"); tick() })
}

// qualityTestDatabase accepts a freshly provisioned fixture on hosts where
// the test process has no Docker socket. Only an empty dedicated database is
// eligible for the embedded migrations. The default uses Testcontainers.
func qualityTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TOKENROUTER_QUALITY_TEST_DSN")
	if dsn == "" {
		return postgrescontainer.New(t)
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var fixture bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT current_database()='tokenrouter_m2a_contract'
 AND NOT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public')`).Scan(&fixture))
	require.True(t, fixture, "expected an empty tokenrouter_m2a_contract database")
	require.NoError(t, postgresinfra.ApplyMigrations(t.Context(), db, migrations.FS))
	return db
}

// GetGroup supplies the single group selected by the HTTP scenario.
func (p qualityPreviewPool) GetGroup(context.Context, int64) (*routing.Group, error) {
	return p.group, nil
}

// GetProvider resolves the target from the same candidate records as the gateway.
func (p qualityPreviewPool) GetProvider(ctx context.Context, id int64) (*gatewayprovider.ExecutionProvider, error) {
	return p.GetByID(ctx, id)
}

// ListProvidersForSchedulerScoreFilter returns the selected OpenAI group's candidates.
func (p qualityPreviewPool) ListProvidersForSchedulerScoreFilter(ctx context.Context, _, _, _, _ string, id int64, _ string) ([]gatewayprovider.ExecutionProvider, error) {
	return p.ListSchedulableByGroupIDAndPlatform(ctx, id, "openai")
}

// ListSchedulableProvidersForAdvancedSchedulerScore shares the group pool with scoring.
func (p qualityPreviewPool) ListSchedulableProvidersForAdvancedSchedulerScore(ctx context.Context, _ *int64, _ string) ([]gatewayprovider.ExecutionProvider, error) {
	return p.ListSchedulableByGroupIDAndPlatform(ctx, p.group.ID, "openai")
}
