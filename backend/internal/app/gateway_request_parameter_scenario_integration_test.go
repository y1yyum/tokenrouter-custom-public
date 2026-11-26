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
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/apikey/testkit"
	"github.com/TokenFlux/TokenRouter/internal/config"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/searchtools"
	gatewaytestkit "github.com/TokenFlux/TokenRouter/internal/gateway/testkit"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry/logging"
	"github.com/TokenFlux/TokenRouter/internal/ops"
	opsprovider "github.com/TokenFlux/TokenRouter/internal/ops/provider"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/server/middleware"
)

// gateway_request_parameter_scenario_integration_test.go exercises
// openai_text_{messages,chat,responses}.go, gateway/forward/conversion.go, and
// ops_capture.go through public POST handlers. It pairs field-dependent mock
// rejections with successful controls and records the exact outbound fields
// and persisted diagnostics to locate each failure in the local build.

const (
	// Fixture identities distinguish selected provider and wire attribution
	// while keeping credentials and requests confined to the test listeners.
	parameterFixtureGroup    = 4515
	parameterFixtureUser     = 1755
	parameterFixtureKey      = 1855
	parameterFixtureProvider = 9955
	parameterFixtureToken    = "parameter-fixture-client"
	parameterMessagesModel   = "claude-sonnet-4-5-20250929"
	parameterMockRequestID   = "parameter-fixture-upstream"

	// The deadline bounds the test's HTTP request and its diagnostic queue drain.
	parameterFixtureTimeout = 10 * time.Second
)

// parameterProbe describes one pair of requests for a field-dependent rejection.
type parameterProbe struct {
	name      string // case name and history reference
	inbound   string // client protocol URL path
	outbound  string // provider protocol URL path
	model     string // original client model
	stream    bool   // requested client streaming
	bad       string // rejected client field JSON
	good      string // control field JSON
	wireField string // GJSON path inspected upstream
	badWire   string // literal rejected JSON value
	goodWire  string // literal control JSON value
	// badCompanions are checked on the rejected wire body to tell derived
	// values from properties the client explicitly supplied in its request.
	badCompanions map[string]string
	upCode        string // synthetic upstream error code
	upMessage     string // synthetic upstream error text
}

// parameterReceipt records the reportable part of one actual outbound request.
type parameterReceipt struct {
	method string // provider request method
	path   string // provider URL path
	body   []byte // raw provider JSON
}

// parameterReply captures the client and wire results for one logical request.
type parameterReply struct {
	id          string             // TokenRouter request identifier
	status      int                // client HTTP status
	contentType string             // client response media type
	body        []byte             // complete client response
	receipts    []parameterReceipt // ordered upstream HTTP arrivals
	forwardLogs []string           // gateway forwarding error details
}

// TestGatewayRequestParameterRejections drives five parameter families through
// three HTTP handlers. It checks actual bridge output against field-dependent
// mock rejections and exercises gateway rejections before any upstream send.
func TestGatewayRequestParameterRejections(t *testing.T) {
	db := qualityTestDatabase(t)
	for _, probe := range parameterProbes() {
		t.Run(probe.name, func(t *testing.T) {
			handler := func(w http.ResponseWriter, r *http.Request, body []byte) {
				if gjson.GetBytes(body, probe.wireField).Raw == probe.badWire {
					writeParameterFixtureRejection(t, w, probe)
					return
				}
				writeParameterFixtureSuccess(t, w, r, body)
			}
			for _, candidate := range []struct {
				name   string // rejected or accepted branch
				fields string // incoming JSON members
				wire   string // expected outbound member value
				status int    // expected client HTTP status
			}{
				{"rejected", probe.bad, probe.badWire, http.StatusBadRequest},
				{"control", probe.good, probe.goodWire, http.StatusOK},
			} {
				t.Run(candidate.name, func(t *testing.T) {
					payload := parameterPayload(probe.inbound, probe.model, probe.stream, candidate.fields)
					reply := runParameterRequest(t, db, probe.inbound, probe.outbound, payload, handler)
					require.Equal(t, candidate.status, reply.status, "%s", reply.body)
					require.Len(t, reply.receipts, 1)
					sent := reply.receipts[0]
					require.Equal(t, http.MethodPost, sent.method)
					require.Equal(t, probe.outbound, sent.path)
					require.Equal(t, candidate.wire, gjson.GetBytes(sent.body, probe.wireField).Raw)
					require.Equal(t, probe.stream || probe.inbound != probe.outbound, gjson.GetBytes(sent.body, "stream").Bool())
					require.Equal(t, parameterWireModel(probe.outbound, probe.model), gjson.GetBytes(sent.body, "model").String())
					if candidate.status == http.StatusOK {
						require.Contains(t, string(reply.body), "ok-mixed")
						if probe.stream {
							require.Contains(t, reply.contentType, "text/event-stream")
						}
					} else {
						require.True(t, json.Valid(reply.body), "%s", reply.body)
						require.Equal(t, probe.upMessage, gjson.GetBytes(reply.body, "error.message").String())
						for field, want := range probe.badCompanions {
							require.Equal(t, want, gjson.GetBytes(sent.body, field).Raw, field)
						}
						if probe.outbound == gatewayhttp.EndpointResponses {
							require.Equal(t, probe.upCode, gjson.GetBytes(reply.body, "error.code").String())
							require.Equal(t, probe.wireField, gjson.GetBytes(reply.body, "error.param").String())
						}
					}
					assertParameterOps(t, db, reply, probe.inbound, probe.model, probe.stream)
				})
			}
		})
	}

	// Malformed inputs exercise actual local code while the listener stands ready
	// to return success if a request reaches it.
	for _, tc := range []struct {
		name      string // local rejection site
		path      string // client protocol URL path
		fields    string // incoming JSON members
		status    int    // expected client HTTP status
		message   string // expected client error message
		forwarded string // expected diagnostic beyond client output
	}{
		{"entry/messages_duplicate_thinking", gatewayhttp.EndpointMessages, `,"thinking":{"type":"adaptive"},"thinking":{"type":"enabled","budget_tokens":1024}`, http.StatusBadRequest, `Request body contains duplicate JSON key "thinking"`, ""},
		{"entry/chat_duplicate_tool_choice", gatewayhttp.EndpointChatCompletions, `,"tool_choice":"required","tool_choice":"auto"`, http.StatusBadRequest, `Request body contains duplicate JSON key "tool_choice"`, ""},
		{"entry/responses_duplicate_temperature", gatewayhttp.EndpointResponses, `,"temperature":0,"temperature":0.5`, http.StatusBadRequest, `Request body contains duplicate JSON key "temperature"`, ""},
		{
			"bridge/responses_temperature_type", gatewayhttp.EndpointResponses, `,"temperature":"cold"`, http.StatusBadGateway, "Upstream request failed",
			"parse responses request: json: cannot unmarshal string into Go struct field ResponsesRequest.temperature",
		},
		{
			"bridge/responses_effort", gatewayhttp.EndpointResponses, `,"reasoning":{"effort":"ultra"}`, http.StatusBadGateway, "Upstream request failed",
			`convert responses to anthropic: reasoning effort "ultra" is not supported`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const model = "claude-haiku-5-5"
			payload := parameterPayload(tc.path, model, false, tc.fields)
			reply := runParameterRequest(t, db, tc.path, gatewayhttp.EndpointMessages, payload, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				mixedUpstreamResponse(w, r, false)
			})
			require.Equal(t, tc.status, reply.status, "%s", reply.body)
			require.Equal(t, tc.message, gjson.GetBytes(reply.body, "error.message").String())
			require.Empty(t, reply.receipts)
			if tc.forwarded == "" {
				require.Empty(t, reply.forwardLogs)
			} else {
				require.Len(t, reply.forwardLogs, 1)
				require.Contains(t, reply.forwardLogs[0], tc.forwarded)
			}
			assertParameterOps(t, db, reply, tc.path, model, false)
		})
	}

	t.Run("fixed_400_control", func(t *testing.T) {
		const model = "claude-haiku-5-5"
		const path = gatewayhttp.EndpointResponses
		probe := parameterProbe{wireField: "temperature", upMessage: "fixture does not support non-default temperature"}
		reply := runParameterRequest(t, db, path, gatewayhttp.EndpointMessages, parameterPayload(path, model, true, ""),
			func(w http.ResponseWriter, _ *http.Request, _ []byte) { writeParameterFixtureRejection(t, w, probe) })
		require.Equal(t, http.StatusBadRequest, reply.status)
		require.Equal(t, probe.upMessage, gjson.GetBytes(reply.body, "error.message").String())
		require.Len(t, reply.receipts, 1)
		require.False(t, gjson.GetBytes(reply.receipts[0].body, "temperature").Exists())
		assertParameterOps(t, db, reply, path, model, true)
	})
}

// parameterProbes specifies mock predicates with one matching control each.
// History identifiers select protocol, requested alias and client stream
// mode. The field values and resolved Claude model are local test choices.
func parameterProbes() []parameterProbe {
	// These request fragments carry synthetic tool and schema definitions
	// in the encoding required by each of the three public text protocols.
	const (
		messages      = gatewayhttp.EndpointMessages
		chat          = gatewayhttp.EndpointChatCompletions
		responses     = gatewayhttp.EndpointResponses
		adaptive      = "fixture requires adaptive thinking"
		temp          = "fixture does not support non-default temperature"
		forced        = "fixture does not support forced tool_choice"
		messageTool   = `,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]`
		chatTool      = `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]`
		responsesTool = `,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]`
		format        = `,"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"answer":{"type":%s}}}}}`
		function      = `,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"query":{"type":%s}}}}]`
		chatFunction  = `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":%s}}}}}]`
	)
	return []parameterProbe{
		{
			name: "adaptive/messages", inbound: messages, outbound: messages, model: "claude-fable-5-2",
			bad: `,"thinking":{"type":"enabled","budget_tokens":1024}`, good: `,"thinking":{"type":"adaptive"}`,
			wireField: "thinking.type", badWire: `"enabled"`, goodWire: `"adaptive"`,
			badCompanions: map[string]string{"thinking.budget_tokens": "1024"}, upMessage: adaptive,
		},
		{
			name: "adaptive/chat", inbound: chat, outbound: messages, model: "claude-fable-5-2",
			bad:       `,"reasoning_effort":"medium"`,
			wireField: "thinking.type", badWire: `"enabled"`,
			badCompanions: map[string]string{"thinking.budget_tokens": "4096", "output_config.effort": `"medium"`}, upMessage: adaptive,
		},
		{
			name: "temperature/messages", inbound: messages, outbound: messages, model: "claude-haiku-5-5",
			bad: `,"temperature":0`, wireField: "temperature", badWire: "0", upMessage: temp,
		},
		{
			name: "temperature/chat", inbound: chat, outbound: messages, model: "claude-haiku-5-5",
			bad: `,"temperature":0.5`, wireField: "temperature", badWire: "0.5", upMessage: temp,
		},
		{
			name: "temperature/responses_6094_sample_1", inbound: responses, outbound: messages, model: "claude-haiku-5-5", stream: true,
			bad: `,"temperature":0.5`, wireField: "temperature", badWire: "0.5", upMessage: temp,
		},
		{
			name: "temperature/responses_6094_sample_2", inbound: responses, outbound: messages, model: "claude-haiku-5-5", stream: true,
			bad: `,"temperature":0`, wireField: "temperature", badWire: "0", upMessage: temp,
		},
		{
			name: "tool_choice/messages_16780", inbound: messages, outbound: messages, model: "claude-fable-5-2",
			bad: messageTool + `,"tool_choice":{"type":"any"}`, good: messageTool + `,"tool_choice":{"type":"auto"}`,
			wireField: "tool_choice.type", badWire: `"any"`, goodWire: `"auto"`, upMessage: forced,
		},
		{
			name: "tool_choice/chat", inbound: chat, outbound: messages, model: "claude-opus-5-5",
			bad: chatTool + `,"tool_choice":"required"`, good: chatTool + `,"tool_choice":"auto"`,
			wireField: "tool_choice.type", badWire: `"any"`, goodWire: `"auto"`, upMessage: forced,
		},
		{
			name: "tool_choice/responses_32663", inbound: responses, outbound: messages, model: "claude-opus-5-5",
			bad: responsesTool + `,"tool_choice":{"type":"function","name":"lookup"}`, good: responsesTool + `,"tool_choice":"auto"`,
			wireField: "tool_choice.type", badWire: `"tool"`, goodWire: `"auto"`,
			badCompanions: map[string]string{"tool_choice.name": `"lookup"`, "tools.0.name": `"lookup"`}, upMessage: forced,
		},
		{
			name: "tool_choice/responses_32664", inbound: responses, outbound: messages, model: "claude-sonnet-5-5",
			bad: responsesTool + `,"tool_choice":"required"`, good: responsesTool + `,"tool_choice":"auto"`,
			wireField: "tool_choice.type", badWire: `"any"`, goodWire: `"auto"`, upMessage: forced,
		},
		{
			name: "response_schema/responses_5651", inbound: responses, outbound: responses, model: "gpt-6-luna", stream: true,
			bad: fmt.Sprintf(format, `42`), good: fmt.Sprintf(format, `"string"`),
			wireField: "text.format.schema.properties.answer.type", badWire: "42", goodWire: `"string"`,
			upCode: "invalid_json_schema", upMessage: "Invalid schema for response_format 'answer': type cannot be numeric.",
		},
		{
			name: "function_schema/responses_8760", inbound: responses, outbound: responses, model: "gpt-6-luna",
			bad: fmt.Sprintf(function, `42`), good: fmt.Sprintf(function, `"string"`),
			wireField: "tools.0.parameters.properties.query.type", badWire: "42", goodWire: `"string"`,
			upCode: "invalid_function_parameters", upMessage: "Invalid schema for function 'lookup': type cannot be numeric.",
		},
		{
			name: "function_schema/responses_6286", inbound: responses, outbound: responses, model: "gpt-6-luna", stream: true,
			bad: fmt.Sprintf(function, `42`), good: fmt.Sprintf(function, `"string"`),
			wireField: "tools.0.parameters.properties.query.type", badWire: "42", goodWire: `"string"`,
			upCode: "invalid_function_parameters", upMessage: "Invalid schema for function 'lookup': type cannot be numeric.",
		},
		{
			name: "function_schema/chat_36712", inbound: chat, outbound: chat, model: "deepseek-v4.1-flash-1017", stream: true,
			bad: fmt.Sprintf(chatFunction, `42`), good: fmt.Sprintf(chatFunction, `"string"`),
			wireField: "tools.0.function.parameters.properties.query.type", badWire: "42", goodWire: `"string"`,
			upCode: "invalid_function_parameters", upMessage: "Invalid schema for function 'lookup': type cannot be numeric.",
		},
	}
}

// parameterPayload supplies minimal public request envelopes for each client protocol.
func parameterPayload(path, model string, stream bool, fields string) string {
	input := `"messages":[{"role":"user","content":"hello"}]`
	switch path {
	case gatewayhttp.EndpointMessages:
		input += `,"max_tokens":4096`
	case gatewayhttp.EndpointResponses:
		input = `"input":"hello"`
	}
	return fmt.Sprintf(`{"model":%q,%s,"stream":%t%s}`, model, input, stream, fields)
}

// parameterWireModel maps Messages probes to the same local transport target.
func parameterWireModel(path, requested string) string {
	if path == gatewayhttp.EndpointMessages {
		return parameterMessagesModel
	}
	return requested
}

// writeParameterFixtureRejection returns a synthetic upstream error with a
// correlation header. The fixture's caller decides whether the request body
// meets the predicate for writing this response.
func writeParameterFixtureRejection(t *testing.T, w http.ResponseWriter, probe parameterProbe) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", parameterMockRequestID)
	w.WriteHeader(http.StatusBadRequest)
	_, err := fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q,"code":%q,"param":%q}}`,
		probe.upMessage, probe.upCode, probe.wireField)
	if err != nil {
		t.Error(err)
	}
}

// writeParameterFixtureSuccess responds with a protocol-shaped success.
// The shared mixed-protocol fixture supplies Messages and Responses output.
// Native Chat requests asking for SSE get a completion chunk and the
// terminal marker so the client stream contract is exercised.
func writeParameterFixtureSuccess(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte) {
	t.Helper()
	if r.URL.Path != gatewayhttp.EndpointChatCompletions || !gjson.GetBytes(body, "stream").Bool() {
		mixedUpstreamResponse(w, r, false)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := `{"id":"chat-mixed","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":"ok-mixed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`
	_, err := fmt.Fprintf(w, "data: "+chunk+"\n\ndata: [DONE]\n\n", gjson.GetBytes(body, "model").String())
	if err != nil {
		t.Error(err)
	}
}

// runParameterRequest sends one authenticated POST through app assembly and
// stores any Ops error in the supplied migrated test database. It returns
// after both HTTP listeners and the Ops queue have finished, so the receive
// count and database row are ready for assertions. Optional configuration
// binds extra policy services without changing the default source probes.
func runParameterRequest(t *testing.T, db *sql.DB, inbound, outbound, payload string, respond func(http.ResponseWriter, *http.Request, []byte), configure ...func(*gatewayHTTPFixtureInput)) parameterReply {
	t.Helper()
	return runParameterRequestWithProviders(t, db, inbound, outbound, payload, respond, nil, configure...)
}

// runParameterRequestWithProviders lets wire tests declare exact capabilities,
// select passthrough, or attach additional candidates to the same listener.
func runParameterRequestWithProviders(t *testing.T, db *sql.DB, inbound, outbound, payload string, respond func(http.ResponseWriter, *http.Request, []byte), configureProviders func(*provider.Record) []*provider.Record, configure ...func(*gatewayHTTPFixtureInput)) parameterReply {
	t.Helper()
	return runParameterRequestWithClient(t, db, inbound, outbound, payload, respond, configureProviders, nil, nil, configure...)
}

// runParameterRequestWithClient drives the same authenticated HTTP fixture with
// optional gateway middleware and a client-side consumer. Disconnect scenarios
// can stop reading midstream and coordinate with the actual request context
// before allowing the upstream to finish its response.
func runParameterRequestWithClient(t *testing.T, db *sql.DB, inbound, outbound, payload string, respond func(http.ResponseWriter, *http.Request, []byte), configureProviders func(*provider.Record) []*provider.Record, before gin.HandlerFunc, read func(*http.Response, context.CancelFunc) ([]byte, error), configure ...func(*gatewayHTTPFixtureInput)) parameterReply {
	t.Helper()
	var mu sync.Mutex
	var receipts []parameterReceipt
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read provider request: %v", err)
			return
		}
		// Keep a complete receipt before responding so additional sends and
		// locally generated errors cannot hide behind the returned client status.
		mu.Lock()
		receipts = append(receipts, parameterReceipt{method: r.Method, path: r.URL.Path, body: body})
		mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		respond(w, r, body)
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{}
	cfg.Billing.FallbackRateMultiplier = 1
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	group := &routing.Group{
		ID: parameterFixtureGroup, Hydrated: true, Name: "parameter-fixture", Status: "active", RateMultiplier: 1,
		AllowedProtocols: []protocol.ProtocolID{protocol.ProtocolAnthropicMessages, protocol.ProtocolOpenAIChatCompletions, protocol.ProtocolOpenAIResponses},
	}
	clientModel := gjson.Get(payload, "model").String()
	target := &provider.Record{
		ID: parameterFixtureProvider, Name: "parameter-fixture", Platform: provider.PlatformOpenAI, Type: provider.ProviderTypeAPIKey,
		Status: "active", Schedulable: true, Priority: 1, Concurrency: 1, GroupIDs: []int64{group.ID},
		Credentials: map[string]any{
			"api_key": "parameter-fixture-upstream", "base_url": upstream.URL,
			"model_mapping": map[string]any{clientModel: parameterWireModel(outbound, clientModel)},
		},
	}
	switch outbound {
	case gatewayhttp.EndpointMessages:
		target.Platform = provider.PlatformAnthropic
	case gatewayhttp.EndpointChatCompletions:
		target.Extra = map[string]any{provider.ExtraKeyTextRouteMode: string(provider.TextRouteModeForceChatCompletions)}
	}
	targets := []*provider.Record{target}
	if configureProviders != nil {
		targets = configureProviders(target)
	}
	pool := &mixedHTTPProviders{}
	for _, target := range targets {
		pool.values = append(pool.values, *gatewayprovider.NewExecutionProvider(target))
	}
	transport := provideHTTPUpstream(cfg)
	t.Cleanup(transport.CloseIdleConnections)
	source, choices, credentials := newOpenAIExecutionAndSelectionFixture(pool, nil, cfg, nil, nil, nil,
		transport, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	recording := gatewaytestkit.NewRecording(&gatewaytestkit.UsageLogStore{Inserted: true}, &gatewaytestkit.SettlementStore{}, nil, false)
	recording.Options.DefaultMultiplier = 1
	source.Recorder = recording.Core(nil, true)
	search := searchtools.NewEmulator(mixedHTTPNoSearch{}, nil, nil, nil, nil, nil)
	entClient := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	readers := &gatewayprovider.RuntimeReaders{Gateway: provideGatewaySettings(provideSettingsStore(entClient))}
	messages := provideMessagesExecution(&provider.MessageCredentialSource{}, nil, transport, nil, nil, readers, nil,
		search, nil, nil, pool, &provider.DeferredService{}, cfg, nil, nil)
	native := &gatewayhttp.UnifiedTextExecutor{OpenAI: source.Responses, Anthropic: messages}
	eligibility := newBillingEligibilityFixture(cfg)
	t.Cleanup(eligibility.Stop)
	user := &identity.User{ID: parameterFixtureUser, Balance: 100, Status: "active"}
	key := &apikey.APIKey{
		ID: parameterFixtureKey, Key: parameterFixtureToken, Status: "active",
		UserID: user.ID, User: user, GroupID: &group.ID, Group: group, BillingMode: "balance",
	}
	keys := testkit.NewService(&openCodeHTTPKeys{key: key}, nil, nil, nil, nil, nil, cfg)
	t.Cleanup(keys.Stop)
	opsService := ops.NewOpsService(provideOpsRepository(db), nil, nil, nil, nil, nil, nil, opsprovider.LogControl{})
	queue := ops.NewErrorLogQueue(ops.ErrorLogQueueOptions{})
	// Cleanup runs after the gateway listener closes. A fresh context lets the
	// queue finish writing even when the test or its HTTP request was canceled.
	drain := func() {
		ctx, cancel := context.WithTimeout(context.Background(), parameterFixtureTimeout)
		defer cancel()
		require.NoError(t, queue.Shutdown(ctx))
	}
	t.Cleanup(drain)
	input := gatewayHTTPFixtureInput{
		Source: source, Native: native, Choices: choices, Credentials: credentials, Config: cfg,
		Funding: newFundingAdmissionFixture(eligibility, cfg), Keys: keys, Ops: opsService, Queue: queue,
		Concurrency: provideOpenAIHTTPResources(scheduler.NewConcurrencyService(nil), cfg).Concurrency,
	}
	for _, apply := range configure {
		apply(&input)
	}
	endpoints := newGatewayHTTPEndpoints(input)
	auth := provideAPIKeyAuth(keys, nil, cfg)
	guards := gatewayhttp.NewRouteGuards(legacyRouteMiddleware(auth, keys, nil, opsService, nil, cfg))
	router := gin.New()
	logCore, diagnostics := observer.New(zap.WarnLevel)
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logging.IntoContext(c.Request.Context(), zap.New(logCore)))
	})
	router.Use(middleware.ClientRequestID(), gatewayhttp.TextRequestTiming(),
		gatewayhttp.OpsErrorLoggerMiddleware(opsService, queue, provideOpsObservationAccess()),
		gatewayhttp.InboundEndpointMiddleware(), gin.HandlerFunc(auth))
	if before != nil {
		router.Use(before)
	}
	router.POST(gatewayhttp.EndpointMessages, guards.RequireGroupClientProtocol(protocol.ProtocolAnthropicMessages, gatewayhttp.GroupClientProtocolErrorAnthropic), endpoints.Messages)
	router.POST(gatewayhttp.EndpointChatCompletions, guards.RequireGroupClientProtocol(protocol.ProtocolOpenAIChatCompletions, gatewayhttp.GroupClientProtocolErrorOpenAI), endpoints.ChatCompletions)
	router.POST(gatewayhttp.EndpointResponses, guards.RequireGroupClientProtocol(protocol.ProtocolOpenAIResponses, gatewayhttp.GroupClientProtocolErrorOpenAI), endpoints.Responses)

	gateway := httptest.NewServer(router)
	t.Cleanup(gateway.Close)
	ctx, cancel := context.WithTimeout(t.Context(), parameterFixtureTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+inbound, strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+parameterFixtureToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := gateway.Client().Do(req)
	require.NoError(t, err)
	var body []byte
	if read == nil {
		body, err = io.ReadAll(res.Body)
	} else {
		body, err = read(res, cancel)
	}
	// Join the public server before draining Ops because response EOF can precede
	// the middleware's last enqueue. Close the upstream after the handler exits.
	require.NoError(t, res.Body.Close())
	require.NoError(t, err)
	gateway.Close()
	upstream.Close()
	drain()
	reply := parameterReply{
		id: res.Header.Get("X-TokenRouter-Request-ID"), status: res.StatusCode,
		contentType: res.Header.Get("Content-Type"), body: body, receipts: receipts,
	}
	for _, entry := range diagnostics.FilterMessageSnippet(".forward_failed").All() {
		if diagnostic, ok := entry.ContextMap()["error"].(string); ok {
			reply.forwardLogs = append(reply.forwardLogs, diagnostic)
		}
	}
	t.Logf("inbound=%s json=%s status=%d content_type=%s reply=%s hits=%d", inbound, payload, reply.status, reply.contentType, body, len(receipts))
	for _, diagnostic := range reply.forwardLogs {
		t.Logf("gateway forward error=%s", diagnostic)
	}
	for _, sent := range receipts {
		t.Logf("upstream=%s %s json=%s", sent.method, sent.path, sent.body)
	}
	return reply
}

// assertParameterOps reads persisted attribution for the local request ID.
// Success controls must leave no Ops error row. Failure diagnostics preserve
// absent status and attempt data because those gaps matter for attribution.
func assertParameterOps(t *testing.T, db *sql.DB, reply parameterReply, path, model string, stream bool) {
	t.Helper()
	require.NotEmpty(t, reply.id)
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM ops_error_logs WHERE request_id=$1`, reply.id).Scan(&count))
	t.Logf("ops request=%s rows=%d", reply.id, count)
	if reply.status == http.StatusOK {
		require.Zero(t, count)
		return
	}
	require.Equal(t, 1, count)
	var status int
	var upstreamStatus, providerID sql.NullInt64
	var groupID int64
	var requestPath, inbound, requested, upstream, mapped, source, phase, attemptsJSON string
	var recordedStream bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT status_code, upstream_status_code, provider_id, group_id, request_path,
		COALESCE(inbound_endpoint,''),
		COALESCE(requested_model,''), COALESCE(upstream_endpoint,''), COALESCE(upstream_model,''), error_source, error_phase,
		stream, COALESCE(upstream_errors,'[]') FROM ops_error_logs WHERE request_id=$1`, reply.id).
		Scan(&status, &upstreamStatus, &providerID, &groupID, &requestPath, &inbound, &requested, &upstream, &mapped, &source, &phase, &recordedStream, &attemptsJSON))
	attempts, err := ops.ParseOpsUpstreamErrors(attemptsJSON)
	require.NoError(t, err)
	t.Logf("ops status=%d phase=%s source=%s requested=%q stream=%t provider=%v upstream=%s mapped=%q upstream_status=%v attempts=%s",
		status, phase, source, requested, recordedStream, providerID, upstream, mapped, upstreamStatus, attemptsJSON)
	require.Equal(t, reply.status, status)
	require.EqualValues(t, parameterFixtureGroup, groupID)
	require.Equal(t, path, requestPath)
	require.Equal(t, path, inbound)
	require.Equal(t, stream, recordedStream)

	if len(reply.receipts) == 0 {
		require.False(t, upstreamStatus.Valid)
		require.Empty(t, attempts)
		require.Empty(t, mapped)
		if status == http.StatusBadRequest {
			require.False(t, providerID.Valid)
			require.Empty(t, requested)
			require.Equal(t, "request", phase)
			require.Equal(t, "client_request", source)
			return
		}
		require.EqualValues(t, parameterFixtureProvider, providerID.Int64)
		require.Equal(t, model, requested)
		require.Equal(t, gatewayhttp.EndpointMessages, upstream)
		require.Equal(t, "upstream", phase)
		require.Equal(t, "upstream_http", source)
		return
	}
	require.Equal(t, model, requested)
	require.EqualValues(t, parameterFixtureProvider, providerID.Int64)
	require.Equal(t, reply.receipts[0].path, upstream)

	if path != upstream {
		// The bridge's ordinary error renderer returns the upstream message.
		// Its 400 response can coexist with a real send and no Ops attempt.
		require.False(t, upstreamStatus.Valid)
		require.Empty(t, attempts)
		require.Empty(t, mapped)
		require.Equal(t, "internal", phase)
		require.Equal(t, "gateway", source)
		require.Equal(t, []string{"upstream error: 400 " + gjson.GetBytes(reply.body, "error.message").String()}, reply.forwardLogs)
		return
	}

	require.Equal(t, "upstream", phase)
	require.Equal(t, "upstream_http", source)
	require.Equal(t, sql.NullInt64{Int64: http.StatusBadRequest, Valid: true}, upstreamStatus)
	require.Len(t, attempts, 1)
	require.Equal(t, http.StatusBadRequest, attempts[0].UpstreamStatusCode)
	require.EqualValues(t, parameterFixtureProvider, attempts[0].ProviderID)
	require.Equal(t, parameterMockRequestID, attempts[0].UpstreamRequestID)
	require.Equal(t, "http_error", attempts[0].Kind)
	require.Equal(t, gjson.GetBytes(reply.body, "error.message").String(), attempts[0].Message)
	if path == gatewayhttp.EndpointMessages {
		require.Empty(t, mapped)
		require.Equal(t, provider.PlatformAnthropic, attempts[0].Platform)
		return
	}
	require.Equal(t, model, mapped)
	require.Equal(t, provider.PlatformOpenAI, attempts[0].Platform)
}
