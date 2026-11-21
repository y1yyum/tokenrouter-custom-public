//go:build integration

package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/apikey"
	keyhttp "github.com/TokenFlux/TokenRouter/internal/apikey/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/apikey/testkit"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	billingpg "github.com/TokenFlux/TokenRouter/internal/billing/postgres"
	"github.com/TokenFlux/TokenRouter/internal/billing/pricing"
	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/gateway/completion"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/provider/messageforward"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	"github.com/TokenFlux/TokenRouter/internal/gateway/searchtools"
	gatewaytestkit "github.com/TokenFlux/TokenRouter/internal/gateway/testkit"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/pkg/timezone"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	usagepg "github.com/TokenFlux/TokenRouter/internal/usage/postgres"
)

// stream_settlement_scenario_integration_test.go exercises HTTP and WS gateway
// execution, completion capture and the PostgreSQL funding and usage writers.
// Both ends of each stream use TCP. A distinct price per bucket makes a wrong
// TTL visible even if the sum of all input counters stays unchanged.

// streamPriceCatalog gives each token class a distinguishable test USD price.
type streamPriceCatalog struct{ nativeCompletionCatalog }

// streamCharge records what crossed the financial transaction boundary. The
// durable store still owns the identity claim, fingerprint and balance mutation.
type streamCharge struct {
	*billingpg.SettlementStore
	seen chan billing.UsageBillingCommand // published after the database returns
}

// Apply records every attempted charge, including any duplicate invocation.
func (s *streamCharge) Apply(ctx context.Context, cmd *billing.UsageBillingCommand) (*billing.UsageBillingApplyResult, error) {
	result, err := s.SettlementStore.Apply(ctx, cmd)
	s.seen <- *cmd
	return result, err
}

// GetModelPricing returns USD per token in the fixed accounting fixture.
func (streamPriceCatalog) GetModelPricing(string) *pricing.CatalogModelPricing {
	return &pricing.CatalogModelPricing{
		InputCostPerToken: .01, OutputCostPerToken: .02, CacheReadInputTokenCost: .003,
		CacheCreationInputTokenCost: .04, CacheCreation5mPrice: new(.04), CacheCreationInputTokenCostAbove1hr: .09, CacheCreation1hPricePresent: true,
		CacheCreation30mPrice: new(.05), CacheCreation12hPrice: new(.11),
	}
}

// TestStreamSettlement runs each sequence against the same migrated stores.
// Balance deltas and per-request dedup rows independently check the funding
// effect, including when two wire terminal events name the same response.
func TestStreamSettlement(t *testing.T) {
	db := qualityTestDatabase(t)
	ctx := t.Context()
	for _, q := range []string{
		"INSERT INTO groups(id,name) VALUES(4301,'stream')",
		"INSERT INTO users(id,email,password_hash,balance) VALUES(1751,'stream@example.invalid','fixture',100)",
		"INSERT INTO api_keys(id,user_id,group_id,name,key,quota,billing_mode) VALUES(1851,1751,4301,'stream','fixture',100,'balance')",
		"INSERT INTO providers(id,name,platform,type) VALUES(9951,'responses','openai','apikey'),(9952,'messages','anthropic','apikey'),(9953,'retry','openai','apikey')",
	} {
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err)
	}
	calendar := timezone.NewCalendar(time.UTC)
	logs := usagepg.NewUsageLogRepositoryWithSQL(ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db))), nativeCompletionSQL{db}, calendar)
	t.Cleanup(logs.StopUsageBatchers)
	funds := &streamCharge{SettlementStore: billingpg.NewSettlementStore(db, calendar, nil), seen: make(chan billing.UsageBillingCommand, 16)}
	r := gatewaytestkit.NewRecording(logs, funds, nil, false)
	r.Options.DefaultMultiplier = 1
	r.Dependencies.Calculator = billing.NewCalculator(streamPriceCatalog{}, billing.CalculatorOptions{})
	r.Dependencies.Prices = billing.NewPriceResolver(nil, r.Dependencies.Calculator, nil, nil, nil)
	recorder := r.Core(nil, true)
	for _, transport := range []string{"responses", "messages", "ws"} {
		scenarios := []string{"late", "duplicate", "retry", "zero", "sparse"}
		if transport != "messages" {
			scenarios = append(scenarios, "alias")
		}
		for _, scenario := range scenarios {
			t.Run(transport+"/"+scenario, func(t *testing.T) {
				var last, priorDedup int64
				var before float64
				require.NoError(t, db.QueryRow("SELECT COALESCE(MAX(id),0) FROM usage_logs").Scan(&last))
				require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM usage_billing_dedup").Scan(&priorDedup))
				require.NoError(t, db.QueryRow("SELECT balance FROM users WHERE id=1751").Scan(&before))
				id := "resp_" + transport + "_" + scenario
				frames := streamFrames(transport, scenario, id)
				if transport == "ws" {
					driveStreamWS(t, recorder, id, scenario, frames)
				} else {
					driveStreamHTTP(t, recorder, id, transport, scenario, frames)
				}
				// One observed row must match one effective funding key. Query unjoined
				// counts too, so extra orphan usage or dedup rows cannot disappear in JOIN.
				var count, claims int
				require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM usage_logs WHERE id>$1", last).Scan(&count))
				require.NoError(t, db.QueryRow("SELECT COUNT(*)-$1::bigint FROM usage_billing_dedup", priorDedup).Scan(&claims))
				require.Equal(t, 1, count)
				require.Equal(t, 1, claims)
				var req, key, storedTTL string
				var pid, gid int
				var buckets [7]int
				var actual, spent, after, writeCost float64
				require.NoError(t, db.QueryRow(`SELECT u.request_id,d.request_id,u.provider_id,u.group_id,u.input_tokens,u.cache_read_tokens,u.cache_creation_5m_tokens,u.cache_creation_1h_tokens,COALESCE(u.cache_creation_30m_tokens,0),COALESCE(u.cache_creation_12h_tokens,0),u.output_tokens,u.actual_cost,u.balance_amount_usd,u.cache_creation_cost,p.balance,
concat(COALESCE(u.cache_creation_30m_tokens::text,'NULL'),'/',COALESCE(u.cache_creation_12h_tokens::text,'NULL'))
FROM usage_logs u JOIN usage_billing_dedup d ON d.request_id=COALESCE(u.billing_key,u.request_id) AND d.api_key_id=u.api_key_id JOIN users p ON p.id=u.billing_user_id WHERE u.id>$1 AND u.api_key_id=1851`, last).Scan(&req, &key, &pid, &gid, &buckets[0], &buckets[1], &buckets[2], &buckets[3], &buckets[4], &buckets[5], &buckets[6], &actual, &spent, &writeCost, &after, &storedTTL))
				t.Logf("SQL request=%s billing=%s key=1851 provider=%d group=%d input/read/5m/1h/30m/12h/output=%v write_usd=%.4f actual_usd=%.4f funded_usd=%.4f wallet_delta=%.4f usage=%d claims=%d stored_30m/12h=%s", req, key, pid, gid, buckets, writeCost, actual, spent, before-after, count, claims, storedTTL)
				var commands []billing.UsageBillingCommand
				for len(funds.seen) > 0 {
					commands = append(commands, <-funds.seen)
				}
				for _, cmd := range commands {
					t.Logf("funding request=%s provider=%d group=%v input=%d read=%d write=%d output=%d amount_usd=%.4f", cmd.RequestID, cmd.ProviderID, *cmd.GroupID, cmd.InputTokens, cmd.CacheReadTokens, cmd.CacheCreationTokens, cmd.OutputTokens, cmd.BillableAmountUSD)
				}
				expected := [7]int{100, 30, 0, 0, 5, 7, 10}
				expectedID := 9951
				if transport == "messages" {
					expected = [7]int{100, 30, 5, 7, 0, 0, 10}
					expectedID = 9952
				}
				if transport == "responses" && scenario == "retry" {
					expectedID = 9953
				}
				if scenario == "zero" {
					expected = [7]int{100, 30, 0, 0, 0, 0, 10}
				}
				if scenario == "sparse" && transport != "messages" {
					expected = [7]int{100, 30, 0, 0, 12, 0, 10}
				}
				write := float64(expected[2])*.04 + float64(expected[3])*.09 + float64(expected[4])*.05 + float64(expected[5])*.11
				amount := 1 + .09 + write + .20
				require.NotEmpty(t, req)
				require.Equal(t, id, key)
				require.Equal(t, expectedID, pid)
				require.Equal(t, 4301, gid)
				require.Len(t, commands, 1)
				cmd := commands[0]
				require.Equal(t, key, cmd.RequestID)
				require.EqualValues(t, pid, cmd.ProviderID)
				require.EqualValues(t, gid, *cmd.GroupID)
				require.Equal(t, []int{expected[0], expected[1], expected[2] + expected[3] + expected[4] + expected[5], expected[6]}, []int{cmd.InputTokens, cmd.CacheReadTokens, cmd.CacheCreationTokens, cmd.OutputTokens})
				require.InDelta(t, amount, cmd.BillableAmountUSD, 1e-8)
				require.Equal(t, expected, buckets)
				require.InDelta(t, write, writeCost, 1e-8)
				for _, v := range []float64{actual, spent, before - after} {
					require.InDelta(t, amount, v, 1e-8)
				}
			})
		}
	}
}

// streamFrames returns cumulative token snapshots and content events in wire
// order. Sparse Responses omit both TTL fields and the final input counter.
// Messages report exclusive input, whereas Responses include both cache buckets.
func streamFrames(transport, scenario, id string) []string {
	if transport == "messages" {
		usage := `"input_tokens":100,"cache_read_input_tokens":30,"cache_creation_input_tokens":12,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":7}`
		if scenario == "zero" {
			usage = `"input_tokens":100,"cache_read_input_tokens":30,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0}`
		}
		start := fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"usage":{%s}}}`, id, usage)
		delta := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`
		if scenario == "late" {
			start = strings.Replace(start, usage, "", 1)
			delta = strings.Replace(delta, `"output_tokens":10`, usage+`,"output_tokens":10`, 1)
		}
		events := []string{start, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`, delta, `{"type":"message_stop"}`}
		if scenario == "duplicate" {
			events = append(events, events[len(events)-1])
		}
		return events
	}
	create := fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"model":"gpt-5.4"}}`, id)
	usage := `"input_tokens":142,"output_tokens":10,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":12,"cache_write_tokens_details":{"ttl_30m_tokens":5,"ttl_12h_tokens":7}}`
	if scenario == "zero" {
		usage = `"input_tokens":130,"output_tokens":10,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":0,"cache_write_tokens_details":{"ttl_30m_tokens":0,"ttl_12h_tokens":0}}`
	}
	if scenario == "sparse" {
		create = strings.Replace(create, `"model":"gpt-5.4"`, `"model":"gpt-5.4","usage":{"input_tokens":142,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":12}}`, 1)
		usage = `"output_tokens":10`
	}
	if scenario == "alias" {
		usage = strings.NewReplacer("input_tokens_details", "prompt_tokens_details", `"input_tokens"`, `"prompt_tokens"`, `"output_tokens"`, `"completion_tokens"`).Replace(usage)
	}
	completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":"gpt-5.4","status":"completed","output":[],"usage":{%s}}}`, id, usage)
	events := []string{create, fmt.Sprintf(`{"type":"response.output_text.delta","response_id":%q,"delta":"ok"}`, id), completed}
	if scenario == "duplicate" {
		events = append(events, completed)
	}
	return events
}

// driveStreamWS drops the first upstream after its content frame for a retry.
// A fresh socket replays the same operation. Each handler exits before the
// SQL assertions so an extra late callback cannot escape the funding check.
func driveStreamWS(t *testing.T, recorder *completion.Recorder, id, scenario string, frames []string) {
	t.Helper()
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, body, err := conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		n := attempts.Add(1)
		t.Logf("upstream WS attempt=%d request=%s", n, body)
		send := frames
		if scenario == "retry" && n == 1 {
			send = frames[:2]
		}
		for _, frame := range send {
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(frame)); err != nil {
				t.Error(err)
				return
			}
			t.Logf("upstream WS %s", frame)
		}
		if len(send) == len(frames) {
			_, _, _ = conn.Read(r.Context())
		}
	}))
	defer upstream.Close()
	runs := 1
	if scenario == "retry" {
		runs = 2
	}
	for i := 0; i < runs; i++ {
		h := newConfiguredWSPassthroughHarness(t, upstream.URL, func(f *gatewayHTTPFixtureInput, p *gatewayprovider.ExecutionProvider, g *routing.Group) {
			f.Moderator = nil
			g.RateMultiplier = 1
			p.Record.GroupIDs = []int64{g.ID}
			f.Source.Recorder = recorder
			t.Cleanup(f.Source.WebSockets.Connections.Close)
		})
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		require.NoError(t, h.clientConn.Write(ctx, coderws.MessageText, []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","input":%q}`, id))))
		ends := 0
		want := 1
		if scenario == "duplicate" {
			want = 2
		}
		for ends < want {
			_, body, err := h.clientConn.Read(ctx)
			if err != nil {
				require.True(t, scenario == "retry" && i == 0, "%v", err)
				break
			}
			t.Logf("client WS %s", body)
			if gjson.GetBytes(body, "type").String() == "response.completed" {
				ends++
			}
		}
		if i == runs-1 {
			require.Equal(t, want, ends)
		}
		h.clientConn.CloseNow()
		select {
		case <-h.handlerDone:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.EqualValues(t, runs, attempts.Load())
}

// driveStreamHTTP flushes each raw SSE event through the selected public
// handler. The Responses retry cuts off a staged prelude on the primary and
// lets gateway failover select the second provider. Messages retries replay a
// client request after the primary closes before reporting any usage.
func driveStreamHTTP(t *testing.T, recorder *completion.Recorder, id, transport, scenario string, frames []string) {
	t.Helper()
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		t.Logf("upstream SSE attempt=%d %s request=%s", n, r.URL.Path, body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", id)
		send := frames
		if scenario == "retry" && n == 1 {
			if transport == "messages" {
				send = []string{fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant","content":[]}}`, id)}
			} else {
				send = frames[:1]
			}
		}
		for _, frame := range send {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(frame, "type").String(), frame)
			w.(http.Flusher).Flush()
			t.Logf("upstream SSE %s", frame)
		}
	}))
	defer upstream.Close()
	path, platform, model, entry := "/v1/responses", "openai", "gpt-5.4", protocol.ProtocolOpenAIResponses
	pids := []int64{9951}
	if transport == "messages" {
		path, platform, model, entry = "/v1/messages", "anthropic", "claude-sonnet-4-5-20250929", protocol.ProtocolAnthropicMessages
		pids = []int64{9952}
	}
	if transport == "responses" && scenario == "retry" {
		pids = append(pids, 9953)
	}
	cfg := &config.Config{}
	cfg.Billing.FallbackRateMultiplier = 1
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.Failover.MaxProviderSwitches = 1
	pool := &mixedHTTPProviders{}
	group := &routing.Group{ID: 4301, Hydrated: true, Name: "stream", Status: "active", RateMultiplier: 1, AllowedProtocols: []protocol.ProtocolID{entry}}
	for i, pid := range pids {
		p := &provider.Record{ID: pid, Name: platform, Platform: platform, Type: "apikey", Status: "active", Schedulable: true, Priority: i + 1, Concurrency: 1, GroupIDs: []int64{group.ID}, Credentials: map[string]any{"api_key": "fixture", "base_url": upstream.URL}}
		pool.values = append(pool.values, *gatewayprovider.NewExecutionProvider(p))
	}
	wire := &mixedHTTPTransport{}
	src, choices, credentials := newOpenAIExecutionAndSelectionFixture(pool, nil, cfg, nil, nil, nil, wire, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	src.Recorder = recorder
	native := &gatewayhttp.UnifiedTextExecutor{OpenAI: src.Responses, Anthropic: gatewayhttp.NewMessagesExecutor(messageforward.NewRuntime(messageforward.Dependencies{Credentials: &provider.MessageCredentialSource{}, Transport: wire, Search: searchtools.NewEmulator(mixedHTTPNoSearch{}, nil, nil, nil, nil, nil), Deferred: &provider.DeferredService{}}, messageforward.Options{Configured: true, AllowInsecureHTTP: true, ResponseReadLimit: 1 << 20}), nil)}
	e := newBillingEligibilityFixture(cfg)
	defer e.Stop()
	keys := testkit.NewService(nil, nil, nil, nil, nil, nil, cfg)
	defer keys.Stop()
	endpoints := newGatewayHTTPEndpoints(gatewayHTTPFixtureInput{Source: src, Native: native, Credentials: credentials, Choices: choices, Config: cfg, Funding: newFundingAdmissionFixture(e, cfg), Keys: keys, Concurrency: provideOpenAIHTTPResources(scheduler.NewConcurrencyService(nil), cfg).Concurrency, MaxSwitches: 1})
	user := &identity.User{ID: 1751, Balance: 100, Status: "active"}
	key := &apikey.APIKey{ID: 1851, UserID: user.ID, User: user, GroupID: &group.ID, Group: group, BillingMode: "balance"}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(keyhttp.ContextKeyAPIKey), key)
		c.Set("gateway_effective_key", key)
		authctx.SetPrincipal(c, identity.Principal{UserID: user.ID}, 0, "")
		c.Request = c.Request.WithContext(requeststate.WithClientProtocol(requeststate.WithGroup(c.Request.Context(), group), entry))
	})
	handler := endpoints.Responses
	payload := fmt.Sprintf(`{"model":%q,"input":%q,"stream":true}`, model, id)
	if transport == "messages" {
		handler = endpoints.Messages
		payload = fmt.Sprintf(`{"model":%q,"max_tokens":20,"messages":[{"role":"user","content":%q}],"stream":true}`, model, id)
	}
	router.POST(path, handler)
	server := httptest.NewServer(router)
	defer server.Close()
	send := func() string {
		req, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+path, strings.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		res, err := server.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		t.Logf("client SSE HTTP=%d body=%s", res.StatusCode, body)
		return string(body)
	}
	body := send()
	if scenario == "retry" && transport == "messages" && attempts.Load() == 1 {
		body = send()
	}
	require.Contains(t, body, "ok")
	want := 1
	if scenario == "retry" {
		want = 2
	}
	require.EqualValues(t, want, attempts.Load())
}
