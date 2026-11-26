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
	"strings"
	"sync"
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
	"github.com/TokenFlux/TokenRouter/internal/billing"
	billingpg "github.com/TokenFlux/TokenRouter/internal/billing/postgres"
	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/gateway/completion"
	"github.com/TokenFlux/TokenRouter/internal/gateway/errorpolicy"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewaypg "github.com/TokenFlux/TokenRouter/internal/gateway/postgres"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/gateway/session"
	gatewaytestkit "github.com/TokenFlux/TokenRouter/internal/gateway/testkit"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/infra/httpclient"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/ops"
	opsprovider "github.com/TokenFlux/TokenRouter/internal/ops/provider"
	"github.com/TokenFlux/TokenRouter/internal/pkg/timezone"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/server/middleware"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
	usagepg "github.com/TokenFlux/TokenRouter/internal/usage/postgres"
)

// gateway_responses_compatibility_scenario_integration_test.go exercises
// openai_responses_execution.go, openaiforward/passthrough.go, ops_capture.go,
// and billing/postgres/settlement.go through a Responses POST. Independent TCP
// receivers record outgoing requests, while the HTTP client retains the full
// response bytes for unary JSON and native SSE comparisons.

const (
	// These fixture identities tie the in-memory gateway projection to the migrated rows used for settlement.
	responsesCompatibilityPath          = "/v1/responses"
	responsesCompatibilityModel         = "gpt-5.4"
	responsesCompatibilityLuna          = "gpt-6-luna"
	responsesCompatibilityFallbackModel = "gpt-5.4-mini"
	responsesCompatibilityA             = 57
	responsesCompatibilityB             = 58
	responsesCompatibilityGroup         = 28
	responsesCompatibilityUser          = 1752
	responsesCompatibilityKey           = 1852
	responsesCompatibilityLimit         = 20

	// Requests and queue drains use a wall-clock bound so a stalled local exchange fails the scenario.
	responsesCompatibilityTimeout = 10 * time.Second
)

// responsesCompatibilityRequest records the HTTP request read by a simulated provider.
type responsesCompatibilityRequest struct {
	method        string      // actual HTTP method
	path          string      // actual URL path
	header        http.Header // received header values
	body          []byte      // received entity bytes
	contentLength int64       // declared entity length
}

// responsesCompatibilityUpstream owns one listener and its ordered request log.
type responsesCompatibilityUpstream struct {
	url string // loopback listener URL

	// The receiver appends to requests under mu before writing a response.
	// Assertions take a snapshot under the same mutex, including when a script blocks.
	mu       sync.Mutex                      // request observation lock
	requests []responsesCompatibilityRequest // ordered received requests
}

// responsesCompatibilityFixture binds two upstream receivers to one gateway listener.
type responsesCompatibilityFixture struct {
	gateway  *httptest.Server                // client-facing TCP listener
	a        *responsesCompatibilityUpstream // primary provider listener
	b        *responsesCompatibilityUpstream // fallback provider listener
	queue    *ops.ErrorLogQueue              // asynchronous Ops writer
	funds    *streamCharge                   // real Apply invocation recorder
	recorder *completion.Recorder            // real pricing and persistence for prepared replay tests
}

// responsesCompatibilityFixtureOptions configures routing and rollout for one fixture.
type responsesCompatibilityFixtureOptions struct {
	bChat          bool                                 // route the fallback through the Chat to Responses adapter
	errorRules     *errorpolicy.ErrorPassthroughService // optional final presentation rules
	worker         *completion.UsageRecordWorkerPool    // optional completion queue under test
	observeRequest func(telemetry.RequestRecord)        // optional request-record observer
	catalog        billing.PriceCatalog                 // optional test price catalogue
	passthrough    bool                                 // both candidates use passthrough
	rewrite        bool                                 // passthrough permits request rewriting
	bFirst         bool                                 // select B before A
	aOnlyPass      bool                                 // keep B on ordinary dispatch
	bOnlyPass      bool                                 // switch only B to passthrough dispatch
	aBasePath      string                               // prefix of A's outbound base URL
	previousID     string                               // prior response bound to A
	previousBID    string                               // prior response bound to B
	policy         *responsecompat.Config               // settings to publish
	compatibility  *responsecompat.Service              // optional independent settings reader
	concurrency    scheduler.ConcurrencyCache           // optional provider slot store
	providerLimit  int                                  // optional capacity for concurrent queue scenarios
	acceptOverride string                               // optional provider Accept value
	tapTransport   func(*http.Request)                  // optional request observation at handoff
	transport      openai.ResponsesTransportOptions     // per-stage test clock overrides
	total          time.Duration                        // logical request test duration
	// switchDirectly scopes the 503 fixtures to candidate switching by setting
	// the provider's own same-provider retry budget to zero.
	switchDirectly bool
	// onlyPrimary removes the fallback from routing, and disableSwitch exhausts
	// the ordinary retry budget before another candidate can be selected.
	onlyPrimary, disableSwitch bool
}

// responsesCompatibilityReply retains the downstream bytes before protocol parsing.
type responsesCompatibilityReply struct {
	status int         // HTTP status code
	header http.Header // response header values
	body   []byte      // unparsed response body
}

// responsesCompatibilityLedger holds the independently counted rows and wallet balance.
type responsesCompatibilityLedger struct {
	usage   int     // persisted usage rows
	claims  int     // persisted dedup rows
	balance float64 // wallet USD balance
}

// TestGatewayResponsesCompatibilityBaseline sends an omitted stream flag,
// explicit false, and explicit true to each dispatch mode. It also exercises a
// recovered upstream error to check the Ops observation and both TCP receivers.
func TestGatewayResponsesCompatibilityBaseline(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, route := range []struct {
		name        string
		passthrough bool
	}{
		{name: "ordinary"},
		{name: "apikey_passthrough", passthrough: true},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name     string
				stream   string
				fallback bool
			}{
				{name: "omitted"},
				{name: "false", stream: "false"},
				{name: "native_stream", stream: "true"},
				{name: "recovered_502", stream: "false", fallback: true},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					idPrefix := "resp_compat_" + route.name + "_" + scenario.name
					responseID := idPrefix + "_a"
					// The 502 arrives before output. Using it as a recovered failure
					// makes an Ops row and a real receipt at B independently observable.
					providerID, vendorModel := responsesCompatibilityA, responsesCompatibilityLuna
					a := responsesCompatibilitySuccess(t, responseID, vendorModel, scenario.stream)
					b := responsesCompatibilitySuccess(t, idPrefix+"_b", responsesCompatibilityFallbackModel, scenario.stream)
					if scenario.fallback {
						a = responsesCompatibilityBadGateway(t)
						providerID, vendorModel = responsesCompatibilityB, responsesCompatibilityFallbackModel
						responseID = idPrefix + "_b"
					}
					fixture := newResponsesCompatibilityFixture(t, db, route.passthrough, a, b)
					before := readResponsesCompatibilityLedger(t, db)
					payload := fmt.Sprintf(`{"model":%q,"input":"return ok","max_tokens":%d}`, responsesCompatibilityModel, responsesCompatibilityLimit)
					if scenario.stream != "" {
						payload = strings.TrimSuffix(payload, "}") + `,"stream":` + scenario.stream + "}"
					}
					reply := fixture.post(t, payload)
					fixture.finish(t)
					assertResponsesCompatibilityRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, scenario.stream, route.passthrough)
					if scenario.fallback {
						assertResponsesCompatibilityRequest(t, fixture.b.snapshot(), responsesCompatibilityB, responsesCompatibilityFallbackModel, scenario.stream, route.passthrough)
					} else {
						require.Empty(t, fixture.b.snapshot())
					}
					assertResponsesCompatibilityReply(t, reply, responseID, scenario.stream == "true")
					assertResponsesCompatibilityFunding(t, db, fixture.funds, reply.header.Get("X-TokenRouter-Request-ID"), vendorModel, providerID, scenario.stream == "true", before)
					assertResponsesCompatibilityOps(t, db, reply, scenario.fallback, route.passthrough)
				})
			}
		})
	}
}

// TestGatewayResponsesCompatibilityForward exercises the Responses rollout on
// two real HTTP paths. The TCP receivers expose wrong modes, mutation carried
// across candidates, and any hidden send after the upstream returns 307.
func TestGatewayResponsesCompatibilityForward(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, passthrough := range []bool{false, true} {
		route := "ordinary"
		if passthrough {
			route = "rewrite_passthrough"
		}
		t.Run(route, func(t *testing.T) {
			for _, scenario := range []struct {
				name       string
				stream     string
				aBasePath  string
				mode       responsecompat.Mode
				failure    string
				response   string
				convert    bool
				clientCode int
			}{
				{name: "omitted", mode: responsecompat.Enforce, convert: true, clientCode: http.StatusOK},
				{name: "false", stream: "false", mode: responsecompat.Enforce, convert: true, clientCode: http.StatusOK},
				{name: "native_true", stream: "true", mode: responsecompat.Enforce, clientCode: http.StatusOK},
				{name: "observe", stream: "false", mode: responsecompat.Observe, clientCode: http.StatusOK},
				{name: "default_off", stream: "false", mode: responsecompat.Off, clientCode: http.StatusOK},
				{name: "actual_endpoint_miss", aBasePath: "/relay", mode: responsecompat.Enforce, clientCode: http.StatusOK},
				{name: "a_to_b", failure: "a", mode: responsecompat.Enforce, convert: true, clientCode: http.StatusOK},
				{name: "b_to_a", stream: "false", failure: "b", mode: responsecompat.Enforce, convert: true, clientCode: http.StatusOK},
				{name: "redirect", mode: responsecompat.Enforce, response: "redirect", convert: true, clientCode: http.StatusBadGateway},
				{name: "matched_legacy_terminal", mode: responsecompat.Enforce, response: "legacy", convert: true, clientCode: http.StatusBadGateway},
				{name: "unmatched_done", aBasePath: "/relay", mode: responsecompat.Enforce, response: "done", clientCode: http.StatusBadGateway},
				{name: "unmatched_legacy_terminal", aBasePath: "/relay", mode: responsecompat.Enforce, response: "legacy", clientCode: http.StatusOK},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					responseID := "resp_forward_" + route + "_" + scenario.name
					wantAStream, wantAAccept := scenario.stream, "application/json"
					if scenario.convert {
						wantAStream, wantAAccept = "true", "text/event-stream"
					}
					a := responsesCompatibilitySuccess(t, responseID, responsesCompatibilityLuna, wantAStream)
					if scenario.convert {
						a = responsesCompatibilityCompletedStream(t, responseID, responsesCompatibilityLuna)
					}
					b := responsesCompatibilitySuccess(t, responseID, responsesCompatibilityFallbackModel, scenario.stream)
					switch scenario.response {
					case "redirect":
						a = func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == responsesCompatibilityPath {
								http.Redirect(w, r, "/unexpected-redirect", http.StatusTemporaryRedirect)
								return
							}
							w.WriteHeader(http.StatusNoContent)
						}
					case "done", "legacy":
						a = responsesCompatibilityLegacyStream(t, scenario.response)
					}
					switch scenario.failure {
					case "a":
						a = responsesCompatibilityPreExecutionRefusal(t)
					case "b":
						b = responsesCompatibilityBadGateway(t)
					}
					fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
						passthrough: passthrough, rewrite: passthrough, bFirst: scenario.failure == "b",
						aBasePath: scenario.aBasePath, policy: responsesCompatibilityPolicy(scenario.mode), switchDirectly: scenario.failure == "a",
					}, a, b)
					before := readResponsesCompatibilityLedger(t, db)
					payload := fmt.Sprintf(`{"model":%q,"input":"return ok","max_tokens":%d}`, responsesCompatibilityModel, responsesCompatibilityLimit)
					if scenario.stream != "" {
						payload = strings.TrimSuffix(payload, "}") + `,"stream":` + scenario.stream + "}"
					}
					reply := fixture.post(t, payload)
					fixture.finish(t)
					assertResponsesCompatibilityWireRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, wantAStream, wantAAccept, scenario.aBasePath+responsesCompatibilityPath, passthrough)
					if scenario.failure != "" {
						assertResponsesCompatibilityRequest(t, fixture.b.snapshot(), responsesCompatibilityB, responsesCompatibilityFallbackModel, scenario.stream, passthrough)
					} else {
						require.Empty(t, fixture.b.snapshot())
					}
					require.Equal(t, scenario.clientCode, reply.status, "%s", reply.body)
					if scenario.clientCode != http.StatusOK {
						require.Contains(t, reply.header.Get("Content-Type"), "application/json")
						require.True(t, json.Valid(reply.body))
						require.True(t, gjson.GetBytes(reply.body, "error").Exists())
						return
					}
					if scenario.response == "legacy" {
						assertResponsesCompatibilityReply(t, reply, "", false)
						return
					}
					assertResponsesCompatibilityReply(t, reply, responseID, scenario.stream == "true")
					providerID, vendorModel := responsesCompatibilityA, responsesCompatibilityLuna
					if scenario.failure == "a" {
						providerID, vendorModel = responsesCompatibilityB, responsesCompatibilityFallbackModel
					}
					assertResponsesCompatibilityFunding(t, db, fixture.funds, reply.header.Get("X-TokenRouter-Request-ID"), vendorModel, providerID, scenario.stream == "true", before)
				})
			}
		})
	}
	t.Run("bound_continuation", func(t *testing.T) {
		for _, passthrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough_%t", passthrough), func(t *testing.T) {
				const responseID = "resp_forward_continued"
				const previousID = "resp_forward_bound"
				fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
					passthrough: passthrough, rewrite: passthrough, bFirst: true,
					previousID: previousID, policy: responsesCompatibilityPolicy(responsecompat.Enforce),
				}, responsesCompatibilityCompletedStream(t, responseID, responsesCompatibilityLuna), responsesCompatibilityBadGateway(t))
				reply := fixture.post(t, fmt.Sprintf(`{"model":"gpt-5.4","input":"return ok","max_tokens":20,"previous_response_id":%q}`, previousID))
				fixture.finish(t)
				requests := fixture.a.snapshot()
				assertResponsesCompatibilityWireRequest(t, requests, responsesCompatibilityA, responsesCompatibilityLuna, "true", "text/event-stream", responsesCompatibilityPath, passthrough)
				require.Equal(t, previousID, gjson.GetBytes(requests[0].body, "previous_response_id").String())
				require.Empty(t, fixture.b.snapshot())
				assertResponsesCompatibilityReply(t, reply, responseID, false)
			})
		}
	})
	t.Run("passthrough_to_ordinary_reasoning", func(t *testing.T) {
		const responseID = "resp_forward_reasoning"
		fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
			passthrough: true, rewrite: true, aOnlyPass: true, policy: responsesCompatibilityPolicy(responsecompat.Enforce), switchDirectly: true,
		}, responsesCompatibilityPreExecutionRefusal(t), responsesCompatibilitySuccess(t, responseID, responsesCompatibilityFallbackModel, "false"))
		reply := fixture.post(t, `{"model":"gpt-5.4","max_tokens":20,"stream":false,"input":[{"type":"message","role":"user","content":"return ok"},{"type":"reasoning","id":"rs_history","encrypted_content":"ENC"},{"type":"message","role":"assistant","content":"prior"}]}`)
		fixture.finish(t)
		a, b := fixture.a.snapshot(), fixture.b.snapshot()
		require.Len(t, a, 1)
		require.Len(t, b, 1)
		require.Equal(t, []string{"text/event-stream"}, a[0].header.Values("Accept"))
		require.Equal(t, "true", gjson.GetBytes(a[0].body, "stream").Raw)
		require.Contains(t, string(a[0].body), `"encrypted_content":"ENC"`)
		require.Equal(t, []string{"application/json"}, b[0].header.Values("Accept"))
		require.Equal(t, "false", gjson.GetBytes(b[0].body, "stream").Raw)
		require.NotContains(t, string(b[0].body), "encrypted_content")
		require.EqualValues(t, 2, gjson.GetBytes(b[0].body, "input.#").Int())
		assertResponsesCompatibilityReply(t, reply, responseID, false)
	})
	t.Run("byte_passthrough", func(t *testing.T) {
		for _, kind := range []string{"done", "legacy"} {
			t.Run(kind, func(t *testing.T) {
				fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
					passthrough: true, policy: responsesCompatibilityPolicy(responsecompat.Enforce),
				}, responsesCompatibilityLegacyStream(t, kind), responsesCompatibilityBadGateway(t))
				reply := fixture.post(t, `{"model":"gpt-5.4","input":"return ok","max_tokens":20,"stream":false}`)
				fixture.finish(t)
				assertResponsesCompatibilityRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, "false", true)
				require.Empty(t, fixture.b.snapshot())
				if kind == "done" {
					require.Equal(t, http.StatusOK, reply.status)
					require.Contains(t, reply.header.Get("Content-Type"), "text/event-stream")
					require.Equal(t, "data: [DONE]\n\n", string(reply.body))
					return
				}
				require.Equal(t, http.StatusOK, reply.status)
				require.Contains(t, reply.header.Get("Content-Type"), "text/event-stream")
				require.True(t, json.Valid(reply.body))
				require.False(t, gjson.GetBytes(reply.body, "id").Exists())
				require.Equal(t, responsesCompatibilityModel, gjson.GetBytes(reply.body, "model").String())
				require.Equal(t, "ok", gjson.GetBytes(reply.body, "output.0.content.0.text").String())
			})
		}
	})
}

// responsesCompatibilityPolicy configures an exact rule for candidate A using the production settings API.
func responsesCompatibilityPolicy(mode responsecompat.Mode) *responsecompat.Config {
	return &responsecompat.Config{
		Mode: mode,
		Rules: []responsecompat.Rule{{
			Tuple: responsecompat.Tuple{
				EffectiveGroupID: responsesCompatibilityGroup,
				ProviderID:       responsesCompatibilityA,
				UpstreamModel:    responsesCompatibilityLuna,
				UpstreamEndpoint: responsesCompatibilityPath,
			},
			Mode: mode, SyncToStream: true, EnforcePercent: 100,
		}},
	}
}

// responsesCompatibilityCompletedStream supplies a full terminal with output
// and usage. Converted requests use it to isolate reader dispatch from delta
// reconstruction, which has its own acceptance step.
func responsesCompatibilityCompletedStream(t *testing.T, id, model string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", id)
		body := fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"object":"response","model":%q,"status":"completed","output":[{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":142,"output_tokens":10,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":12,"cache_write_tokens_details":{"ttl_30m_tokens":5,"ttl_12h_tokens":7}}}}}`, id, model)
		if _, err := fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", body); err != nil {
			t.Errorf("write completed SSE: %v", err)
		}
	}
}

// responsesCompatibilityLegacyStream emits a sentinel without final JSON or
// a terminal JSON object without an ID. The two frames let the test compare
// each preexisting reader contract using the same upstream response bytes.
func responsesCompatibilityLegacyStream(t *testing.T, kind string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frame := "data: [DONE]\n\n"
		if kind == "legacy" {
			body := fmt.Sprintf(`{"type":"response.completed","response":{"object":"response","model":%q,"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":100,"output_tokens":10}}}`, responsesCompatibilityLuna)
			frame = "event: response.completed\ndata: " + body + "\n\n"
		}
		if _, err := io.WriteString(w, frame); err != nil {
			t.Errorf("write legacy SSE: %v", err)
		}
	}
}

// responsesCompatibilityPreExecutionRefusal proves the converted candidate
// rejected admission before execution. The 503 allows the fixture to exercise
// candidate rebuild under the same replay contract as the converted reader.
func responsesCompatibilityPreExecutionRefusal(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := io.WriteString(w, `{"error":{"code":"server_is_overloaded","message":"fixture capacity unavailable","execution_started":false}}`); err != nil {
			t.Errorf("write pre-execution refusal: %v", err)
		}
	}
}

// newResponsesCompatibilityDatabase seeds both routing candidates and a wallet in
// a dedicated migrated database. The serial scenarios start with empty accounting
// and Ops rows, and each fixture collects its own calls and prior wallet balance.
func newResponsesCompatibilityDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := qualityTestDatabase(t)
	for _, query := range []string{
		fmt.Sprintf("INSERT INTO groups(id,name) VALUES(%d,'responses-compatibility')", responsesCompatibilityGroup),
		fmt.Sprintf("INSERT INTO users(id,email,password_hash,balance) VALUES(%d,'compat@example.invalid','fixture',100)", responsesCompatibilityUser),
		fmt.Sprintf("INSERT INTO api_keys(id,user_id,group_id,name,key,quota,billing_mode) VALUES(%d,%d,%d,'compat','fixture',100,'balance')", responsesCompatibilityKey, responsesCompatibilityUser, responsesCompatibilityGroup),
		fmt.Sprintf("INSERT INTO providers(id,name,platform,type) VALUES(%d,'compat-a','openai','apikey'),(%d,'compat-b','openai','apikey')", responsesCompatibilityA, responsesCompatibilityB),
	} {
		_, err := db.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	return db
}

// newResponsesCompatibilityFixture runs the production Responses handler and
// HTTP transport against two independently scripted listeners. The identity
// and group projection are fixture data. The passthrough switch selects the
// provider dispatch path. Completion runs inline, with Ops using its own queue.
// Optional observers run after request IDs and group context are bound.
func newResponsesCompatibilityFixture(t *testing.T, db *sql.DB, passthrough bool, a, b http.HandlerFunc, beforeResponses ...gin.HandlerFunc) *responsesCompatibilityFixture {
	t.Helper()
	return newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{passthrough: passthrough}, a, b, beforeResponses...)
}

// newResponsesCompatibilityConfiguredFixture applies candidate and rollout
// choices while preserving the real Responses handler and transport.
// Settings changes use the same service and store constructor as application
// assembly. Optional observers run after request IDs and group context are bound.
func newResponsesCompatibilityConfiguredFixture(t *testing.T, db *sql.DB, options responsesCompatibilityFixtureOptions, a, b http.HandlerFunc, beforeResponses ...gin.HandlerFunc) *responsesCompatibilityFixture {
	t.Helper()
	fixture := &responsesCompatibilityFixture{
		a: newResponsesCompatibilityUpstream(t, a),
		b: newResponsesCompatibilityUpstream(t, b),
	}
	calendar := timezone.NewCalendar(time.UTC)
	entClient := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	logs := usagepg.NewUsageLogRepositoryWithSQL(entClient, nativeCompletionSQL{db}, calendar)
	t.Cleanup(logs.StopUsageBatchers)
	fixture.funds = &streamCharge{SettlementStore: billingpg.NewSettlementStore(db, calendar, nil), seen: make(chan billing.UsageBillingCommand, 16)}
	recording := gatewaytestkit.NewRecording(logs, fixture.funds, nil, false)
	recording.Options.DefaultMultiplier = 1
	catalog := options.catalog
	if catalog == nil {
		catalog = streamPriceCatalog{}
	}
	recording.Dependencies.Calculator = billing.NewCalculator(catalog, billing.CalculatorOptions{})
	recording.Dependencies.Prices = billing.NewPriceResolver(nil, recording.Dependencies.Calculator, nil, nil, nil)

	cfg := &config.Config{}
	cfg.Billing.FallbackRateMultiplier = 1
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.Failover.MaxProviderSwitches = 1
	var concurrency *scheduler.ConcurrencyService
	if options.concurrency != nil {
		concurrency = scheduler.NewConcurrencyService(options.concurrency)
		cfg.Scheduling.AlternateProviderWait.MaxWaiting = 8
		cfg.Scheduling.AlternateProviderWait.TimeoutSeconds = 8
	}
	acceptOverride := options.acceptOverride
	if acceptOverride == "" {
		acceptOverride = "application/json"
	}
	group := &routing.Group{ID: responsesCompatibilityGroup, Hydrated: true, Name: "compat", Status: "active", RateMultiplier: 1, AllowedProtocols: []protocol.ProtocolID{protocol.ProtocolOpenAIResponses}}
	pool := &mixedHTTPProviders{}
	providerLimit := max(1, options.providerLimit)
	for i, upstream := range []struct {
		id    int64
		url   string
		model string
	}{
		{responsesCompatibilityA, fixture.a.url + options.aBasePath, responsesCompatibilityLuna},
		{responsesCompatibilityB, fixture.b.url, responsesCompatibilityFallbackModel},
	} {
		if i > 0 && options.onlyPrimary {
			break
		}
		priority := i + 1
		if options.bFirst {
			priority = 2 - i
		}
		passthrough := options.passthrough && (!options.aOnlyPass || i == 0) || options.bOnlyPass && i == 1
		record := &provider.Record{
			ID: upstream.id, Name: fmt.Sprintf("compat-%d", upstream.id), Platform: "openai", Type: "apikey",
			Status: "active", Schedulable: true, Priority: priority, Concurrency: providerLimit, GroupIDs: []int64{group.ID},
			Extra: map[string]any{"openai_passthrough": passthrough, "openai_passthrough_allow_rewrite": options.rewrite},
			Credentials: map[string]any{
				"api_key": fmt.Sprintf("compat-upstream-%d", upstream.id), "base_url": upstream.url,
				"model_mapping":           map[string]any{responsesCompatibilityModel: upstream.model},
				"header_override_enabled": true,
				"header_overrides":        map[string]any{"Accept": acceptOverride, "X-Compat-Provider": fmt.Sprint(upstream.id)},
			},
		}
		if options.switchDirectly && i == 0 {
			record.Credentials["pool_mode"] = true
			record.Credentials["pool_mode_retry_count"] = 0
		}
		if options.bChat && i == 1 {
			record.Extra[provider.ExtraKeyTextRouteMode] = string(provider.TextRouteModeForceChatCompletions)
		}
		pool.values = append(pool.values, *gatewayprovider.NewExecutionProvider(record))
	}
	transport := provideHTTPUpstream(cfg)
	t.Cleanup(transport.CloseIdleConnections)
	var upstreamTransport httpclient.UpstreamTransport = transport
	if options.tapTransport != nil {
		upstreamTransport = responsesCompatibilityTransportTap{UpstreamTransport: transport, inspect: options.tapTransport}
	}
	state := session.NewOpenAIWSStateStore(nil)
	if options.previousID != "" {
		require.NoError(t, state.BindResponseProvider(t.Context(), group.ID, options.previousID, responsesCompatibilityA, time.Minute))
		require.NoError(t, state.BindHTTPResponseOwner(t.Context(), group.ID, options.previousID, responsesCompatibilityUser, responsesCompatibilityKey, time.Minute))
	}
	if options.previousBID != "" {
		require.NoError(t, state.BindResponseProvider(t.Context(), group.ID, options.previousBID, responsesCompatibilityB, time.Minute))
		require.NoError(t, state.BindHTTPResponseOwner(t.Context(), group.ID, options.previousBID, responsesCompatibilityUser, responsesCompatibilityKey, time.Minute))
	}
	source, choices, credentials := newOpenAIExecutionAndSelectionFixture(pool, nil, cfg, nil, concurrency, nil, upstreamTransport, nil, nil, nil, nil, nil, nil, nil, nil, nil, state, nil, nil)
	source.Recorder = recording.Core(nil, true)
	fixture.recorder = source.Recorder
	source.Responses.Compatibility = options.compatibility
	if source.Responses.Compatibility == nil {
		source.Responses.Compatibility = responsecompat.New(provideSettingsStore(entClient))
	}
	source.Responses.Cohorts = gatewaypg.NewCompatibilityCohorts(db)
	source.Responses.Output.Options.CompatibilityTransport = options.transport
	source.Responses.Output.Options.CompatibilityTotalTimeout = options.total
	if options.policy != nil {
		// All scenarios update the same isolated database, but each gateway owns
		// its own snapshot. Read the exact revision before publishing a new rule.
		replacement := *options.policy
		status, err := source.Responses.Compatibility.Get(t.Context())
		require.NoError(t, err)
		replacement.Revision = status.Settings.Revision
		_, err = source.Responses.Compatibility.Update(t.Context(), replacement)
		require.NoError(t, err)
	}
	eligibility := newBillingEligibilityFixture(cfg)
	t.Cleanup(eligibility.Stop)
	keys := testkit.NewService(nil, nil, nil, nil, nil, nil, cfg)
	t.Cleanup(keys.Stop)
	opsService := ops.NewOpsService(provideOpsRepository(db), nil, nil, nil, nil, nil, nil, opsprovider.LogControl{})
	fixture.queue = ops.NewErrorLogQueue(ops.ErrorLogQueueOptions{})
	t.Cleanup(func() { fixture.drainOps(t) })
	if concurrency == nil {
		concurrency = scheduler.NewConcurrencyService(nil)
	}
	maxSwitches := 1
	if options.disableSwitch {
		maxSwitches = 0
	}
	endpoints := newGatewayHTTPEndpoints(gatewayHTTPFixtureInput{
		Rules:  options.errorRules,
		Source: source, Choices: choices, Credentials: credentials, Config: cfg,
		Funding: newFundingAdmissionFixture(eligibility, cfg), Keys: keys, Ops: opsService, Queue: fixture.queue, Worker: options.worker,
		Concurrency: provideOpenAIHTTPResources(concurrency, cfg).Concurrency, MaxSwitches: maxSwitches,
	})
	user := &identity.User{ID: responsesCompatibilityUser, Balance: 100, Status: "active"}
	key := &apikey.APIKey{ID: responsesCompatibilityKey, UserID: user.ID, User: user, GroupID: &group.ID, Group: group, BillingMode: "balance"}
	router := gin.New()
	requestIDs := middleware.ClientRequestID()
	if options.observeRequest != nil {
		requestIDs = middleware.RequestLogger(options.observeRequest)
	}
	handlers := []gin.HandlerFunc{
		requestIDs, gatewayhttp.TextRequestTiming(),
		gatewayhttp.OpsErrorLoggerMiddleware(opsService, fixture.queue, provideOpsObservationAccess()), gatewayhttp.InboundEndpointMiddleware(),
		func(c *gin.Context) {
			c.Set(string(keyhttp.ContextKeyAPIKey), key)
			c.Set("gateway_effective_key", key)
			authctx.SetPrincipal(c, identity.Principal{UserID: user.ID}, 0, "")
			ctx := requeststate.WithGroup(c.Request.Context(), group)
			c.Request = c.Request.WithContext(requeststate.WithClientProtocol(ctx, protocol.ProtocolOpenAIResponses))
		},
	}
	handlers = append(handlers, beforeResponses...)
	router.POST(responsesCompatibilityPath, append(handlers, endpoints.Responses)...)
	fixture.gateway = httptest.NewServer(router)
	t.Cleanup(fixture.gateway.Close)
	return fixture
}

// newResponsesCompatibilityUpstream captures a complete request before invoking
// the script. This lets a script pause after receipt while the test inspects
// the request or controls when the listener may finish writing.
func newResponsesCompatibilityUpstream(t *testing.T, script http.HandlerFunc) *responsesCompatibilityUpstream {
	t.Helper()
	upstream := &responsesCompatibilityUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.requests = append(upstream.requests, responsesCompatibilityRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body, contentLength: r.ContentLength})
		upstream.mu.Unlock()
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		script(w, r)
	}))
	t.Cleanup(server.Close)
	upstream.url = server.URL
	return upstream
}

// snapshot returns the request log in receipt order with independent slice
// storage. The recorded request headers and bytes are immutable after capture
// and callers use them as read-only observations.
func (u *responsesCompatibilityUpstream) snapshot() []responsesCompatibilityRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]responsesCompatibilityRequest(nil), u.requests...)
}

// post sends one POST and reads the client response to EOF. The deadline
// bounds the client's wait even if the gateway has unfinished upstream work.
func (f *responsesCompatibilityFixture) post(t *testing.T, payload string) responsesCompatibilityReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), responsesCompatibilityTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.gateway.URL+responsesCompatibilityPath, strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := f.gateway.Client().Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, res.Body.Close()) }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return responsesCompatibilityReply{status: res.StatusCode, header: res.Header.Clone(), body: body}
}

// finish waits for HTTP handlers to return and drains Ops before SQL assertions.
// The fixture serves each scenario until this call, including client EOF and
// any inline completion still running when the response reaches the client.
func (f *responsesCompatibilityFixture) finish(t *testing.T) {
	t.Helper()
	// Join the handlers before closing the queue so a response finishing on the
	// server can still enqueue its last diagnostic after the client reads EOF.
	f.gateway.Close()
	f.drainOps(t)
}

// drainOps closes queue admission and waits for its SQL writes. A fresh
// context permits this operation during test cleanup, after t.Context ends.
func (f *responsesCompatibilityFixture) drainOps(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), responsesCompatibilityTimeout)
	defer cancel()
	require.NoError(t, f.queue.Shutdown(ctx))
}

// responsesCompatibilitySuccess supplies equivalent text and usage in JSON or
// native SSE. The stream repeats its terminal event to match the historical
// stream settlement control and expose duplicate funding submissions.
func responsesCompatibilitySuccess(t *testing.T, id, model, stream string) http.HandlerFunc {
	t.Helper()
	frames := streamFrames("responses", "duplicate", id)
	for i := range frames {
		frames[i] = strings.ReplaceAll(frames[i], responsesCompatibilityModel, model)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", id)
		if stream != "true" {
			w.Header().Set("Content-Type", "application/json")
			body := fmt.Sprintf(`{"id":%q,"object":"response","model":%q,"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":142,"output_tokens":10,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":12,"cache_write_tokens_details":{"ttl_30m_tokens":5,"ttl_12h_tokens":7}}}}`, id, model)
			if _, err := io.WriteString(w, body); err != nil {
				t.Errorf("write upstream JSON: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(frame, "type").String(), frame); err != nil {
				t.Errorf("write upstream SSE: %v", err)
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush upstream SSE: %v", err)
				return
			}
		}
	}
}

// responsesCompatibilityBadGateway scripts a recoverable 502 before any output exists.
func responsesCompatibilityBadGateway(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "compat-a-502")
		w.WriteHeader(http.StatusBadGateway)
		if _, err := io.WriteString(w, `{"error":{"type":"server_error","message":"fixture A unavailable"}}`); err != nil {
			t.Errorf("write upstream 502: %v", err)
		}
	}
}

// assertResponsesCompatibilityRequest checks one provider's actual receipt. The
// ordinary transform translates max_tokens, while API Key passthrough preserves
// that field. Both still apply their provider model mapping and header overrides.
func assertResponsesCompatibilityRequest(t *testing.T, requests []responsesCompatibilityRequest, providerID int, model, stream string, passthrough bool) {
	t.Helper()
	assertResponsesCompatibilityWireRequest(t, requests, providerID, model, stream, "application/json", responsesCompatibilityPath, passthrough)
}

// assertResponsesCompatibilityWireRequest validates the body, media type and
// target path received by one provider. Its provider-specific model and limit
// translations also distinguish the ordinary builder from API Key passthrough.
func assertResponsesCompatibilityWireRequest(t *testing.T, requests []responsesCompatibilityRequest, providerID int, model, stream, accept, path string, passthrough bool) {
	t.Helper()
	require.Len(t, requests, 1)
	req := requests[0]
	t.Logf("upstream provider=%d %s %s accept=%q content-type=%q body=%s", providerID, req.method, req.path, req.header.Values("Accept"), req.header.Get("Content-Type"), req.body)
	require.Equal(t, http.MethodPost, req.method)
	require.Equal(t, path, req.path)
	require.Equal(t, []string{accept}, req.header.Values("Accept"))
	require.Equal(t, "application/json", req.header.Get("Content-Type"))
	require.EqualValues(t, len(req.body), req.contentLength)
	require.Equal(t, fmt.Sprint(providerID), req.header.Get("X-Compat-Provider"))
	require.Equal(t, fmt.Sprintf("Bearer compat-upstream-%d", providerID), req.header.Get("Authorization"))
	require.True(t, json.Valid(req.body))
	require.Equal(t, model, gjson.GetBytes(req.body, "model").String())
	require.Equal(t, "return ok", gjson.GetBytes(req.body, "input").String())
	require.Equal(t, stream, gjson.GetBytes(req.body, "stream").Raw)
	limit, absent := "max_output_tokens", "max_tokens"
	if passthrough {
		limit, absent = absent, limit
	}
	require.EqualValues(t, responsesCompatibilityLimit, gjson.GetBytes(req.body, limit).Int())
	require.False(t, gjson.GetBytes(req.body, absent).Exists())
}

// assertResponsesCompatibilityReply distinguishes one complete JSON document
// from native SSE using the bytes read from the client connection. The model
// returned to the client must use its requested name in both representations.
func assertResponsesCompatibilityReply(t *testing.T, reply responsesCompatibilityReply, id string, stream bool) {
	t.Helper()
	t.Logf("client HTTP=%d content-type=%q bytes=%d body=%s", reply.status, reply.header.Get("Content-Type"), len(reply.body), reply.body)
	require.Equal(t, http.StatusOK, reply.status)
	if stream {
		require.Contains(t, reply.header.Get("Content-Type"), "text/event-stream")
		require.Contains(t, string(reply.body), "data: ")
		require.Contains(t, string(reply.body), `"delta":"ok"`)
		require.Contains(t, string(reply.body), `"type":"response.completed"`)
		require.Contains(t, string(reply.body), id)
		require.Contains(t, string(reply.body), responsesCompatibilityModel)
		require.NotContains(t, string(reply.body), responsesCompatibilityLuna)
		return
	}
	require.Contains(t, reply.header.Get("Content-Type"), "application/json")
	require.True(t, json.Valid(reply.body), "expected exactly one JSON document")
	require.NotContains(t, string(reply.body), "event: ")
	require.NotContains(t, string(reply.body), "data: ")
	require.Equal(t, id, gjson.GetBytes(reply.body, "id").String())
	require.Equal(t, "completed", gjson.GetBytes(reply.body, "status").String())
	require.Equal(t, responsesCompatibilityModel, gjson.GetBytes(reply.body, "model").String())
	require.Equal(t, "ok", gjson.GetBytes(reply.body, "output.0.content.0.text").String())
}

// readResponsesCompatibilityLedger counts usage and dedup rows independently
// so a row without a matching funding key still affects the totals.
func readResponsesCompatibilityLedger(t *testing.T, db *sql.DB) responsesCompatibilityLedger {
	t.Helper()
	var ledger responsesCompatibilityLedger
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT
(SELECT COUNT(*) FROM usage_logs WHERE api_key_id=$1),
(SELECT COUNT(*) FROM usage_billing_dedup WHERE api_key_id=$1),
(SELECT balance FROM users WHERE id=$2)`, responsesCompatibilityKey, responsesCompatibilityUser).
		Scan(&ledger.usage, &ledger.claims, &ledger.balance))
	return ledger
}

// assertResponsesCompatibilityFunding compares the Apply boundary, stored usage,
// dedup key, and wallet movement for the current scenario. The caller supplies
// its pre-request balance after both providers and the billing fixture are ready.
func assertResponsesCompatibilityFunding(t *testing.T, db *sql.DB, funds *streamCharge, requestID, vendorModel string, providerID int, stream bool, before responsesCompatibilityLedger) {
	t.Helper()
	require.Len(t, funds.seen, 1, "Apply calls")
	cmd := <-funds.seen
	var input, output, cacheRead, cacheWrite int
	var storedRequestID, billingKey, fingerprint, upstreamModel string
	var storedProvider int
	var storedStream bool
	var actual, funded float64
	after := readResponsesCompatibilityLedger(t, db)
	usageRows, claims := after.usage-before.usage, after.claims-before.claims
	walletDelta := before.balance - after.balance
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT u.request_id,u.billing_key,d.request_fingerprint,u.provider_id,u.upstream_model,u.stream,u.input_tokens,u.output_tokens,u.cache_read_tokens,u.cache_creation_tokens,u.actual_cost,u.balance_amount_usd
FROM usage_logs u JOIN usage_billing_dedup d ON d.request_id=u.billing_key AND d.api_key_id=u.api_key_id
WHERE u.api_key_id=$1 AND u.billing_key=$2`, responsesCompatibilityKey, cmd.RequestID).
		Scan(&storedRequestID, &billingKey, &fingerprint, &storedProvider, &upstreamModel, &storedStream, &input, &output, &cacheRead, &cacheWrite, &actual, &funded))
	t.Logf("settlement request=%s key=%s provider=%d stream=%t apply=1 usage=%d dedup=%d input=%d output=%d read=%d write=%d wallet_delta=%.4f", storedRequestID, billingKey, storedProvider, storedStream, usageRows, claims, input, output, cacheRead, cacheWrite, walletDelta)
	require.Equal(t, 1, usageRows)
	require.Equal(t, 1, claims)
	require.NotEmpty(t, requestID)
	require.Equal(t, requestID, storedRequestID)
	require.Equal(t, "client:"+requestID, billingKey)
	require.NotEmpty(t, fingerprint)
	require.Equal(t, cmd.RequestFingerprint, fingerprint)
	require.Equal(t, providerID, storedProvider)
	require.EqualValues(t, providerID, cmd.ProviderID)
	require.Equal(t, vendorModel, upstreamModel)
	require.Equal(t, stream, storedStream)
	require.Equal(t, []int{100, 10, 30, 12}, []int{input, output, cacheRead, cacheWrite})
	// The shared stream price catalog uses distinct prices for each cache TTL.
	const amount = 100*.01 + 10*.02 + 30*.003 + 5*.05 + 7*.11
	for _, value := range []float64{cmd.BillableAmountUSD, actual, funded, walletDelta} {
		require.InDelta(t, amount, value, 1e-8)
	}
}

// assertResponsesCompatibilityOps verifies the persisted observation by the
// gateway's local request ID. A clean success has no Ops error row. Recovery
// records one successful HTTP outcome with the failed provider attempt attached.
func assertResponsesCompatibilityOps(t *testing.T, db *sql.DB, reply responsesCompatibilityReply, recovered, passthrough bool) {
	t.Helper()
	id := reply.header.Get("X-TokenRouter-Request-ID")
	require.NotEmpty(t, id)
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM ops_error_logs WHERE request_id=$1", id).Scan(&count))
	if !recovered {
		t.Logf("ops request=%s rows=%d", id, count)
		require.Zero(t, count)
		return
	}
	require.Equal(t, 1, count)
	var status int
	var phase, path, attemptsJSON string
	var stream bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT status_code,error_phase,request_path,stream,upstream_errors FROM ops_error_logs WHERE request_id=$1`, id).
		Scan(&status, &phase, &path, &stream, &attemptsJSON))
	attempts, err := ops.ParseOpsUpstreamErrors(attemptsJSON)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	t.Logf("ops request=%s rows=%d client_status=%d phase=%s upstream_status=%d attempt_provider=%d passthrough=%t", id, count, status, phase, attempts[0].UpstreamStatusCode, attempts[0].ProviderID, attempts[0].Passthrough)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "upstream", phase)
	require.Equal(t, responsesCompatibilityPath, path)
	require.False(t, stream)
	require.Equal(t, http.StatusBadGateway, attempts[0].UpstreamStatusCode)
	require.EqualValues(t, responsesCompatibilityA, attempts[0].ProviderID)
	require.Equal(t, passthrough, attempts[0].Passthrough)
}
