//go:build integration

package app

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/apikey/testkit"
	"github.com/TokenFlux/TokenRouter/internal/app/lifecycle"
	"github.com/TokenFlux/TokenRouter/internal/config"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewaypg "github.com/TokenFlux/TokenRouter/internal/gateway/postgres"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	gatewaytestkit "github.com/TokenFlux/TokenRouter/internal/gateway/testkit"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	provideradapter "github.com/TokenFlux/TokenRouter/internal/provider/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
)

// gateway_parameter_review_scenario_integration_test.go checks independent
// cohort lifecycle through the authenticated HTTP handlers, wire transport and
// PostgreSQL cohort store. The provider listener accepts or explicitly rejects
// parameters so the assertions can distinguish an upstream rejection from a local one.

// TestGatewayParameterReview tests enrollment independence and final rejection attribution.
func TestGatewayParameterReview(t *testing.T) {
	db := qualityTestDatabase(t)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	store := provideSettingsStore(client)
	parameters, responses := requestpolicy.New(store), responsecompat.New(store)
	cohorts := gatewaypg.NewCompatibilityCohorts(db)
	configure := func(input *gatewayHTTPFixtureInput) {
		input.ParameterPolicy = parameters
		input.Source.Responses.Compatibility = responses
		input.Source.Responses.Cohorts = cohorts
	}

	const endpoint = requestpolicy.EndpointResponses
	const model = parameterWireClientModel
	var parameterRevision, responseRevision int64
	for _, both := range []struct {
		first, second bool
	}{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		t.Run(fmt.Sprintf("switches/first_%t_second_%t", both.first, both.second), func(t *testing.T) {
			first := responsecompat.Config{Mode: responsecompat.Off, Revision: responseRevision}
			if both.first {
				first.Mode = responsecompat.Enforce
				first.Rules = []responsecompat.Rule{{
					Tuple: responsecompat.Tuple{EffectiveGroupID: parameterFixtureGroup, ProviderID: parameterFixtureProvider, UpstreamModel: model, UpstreamEndpoint: endpoint},
					Mode:  responsecompat.Enforce, EnforcePercent: 100, SyncToStream: true,
				}}
			}
			_, err := responses.Update(t.Context(), first)
			require.NoError(t, err)
			responseRevision++
			second := requestpolicy.Config{Mode: requestpolicy.ModeOff, Revision: parameterRevision}
			if both.second {
				second = parameterReviewStrictConfig(parameterRevision)
			}
			_, err = parameters.Update(t.Context(), second)
			require.NoError(t, err)
			parameterRevision++

			wantPending := map[int]int{}
			if both.first {
				wantPending[responsecompat.ResponsesPhase] = 1
			}
			if both.second {
				wantPending[responsecompat.ParameterPhase] = 1
			}
			reply := runParameterRequestWithProviders(t, db, endpoint, endpoint,
				parameterPayload(endpoint, model, false, `,"temperature":0`), func(w http.ResponseWriter, r *http.Request, body []byte) {
					rows, err := db.QueryContext(r.Context(), "SELECT phase, count(*) FROM gateway_compatibility_cohorts WHERE outcome IS NULL GROUP BY phase")
					if !assert.NoError(t, err) {
						return
					}
					defer func() { assert.NoError(t, rows.Close()) }()
					pending := map[int]int{}
					for rows.Next() {
						var phase, count int
						if !assert.NoError(t, rows.Scan(&phase, &count)) {
							return
						}
						pending[phase] = count
					}
					assert.NoError(t, rows.Err())
					assert.Equal(t, wantPending, pending)
					assert.Equal(t, both.first, gjson.GetBytes(body, "stream").Bool())
					if both.first {
						responsesSettlementStream(t, []string{settlementCompleted}, false)(w, r)
					} else {
						acceptParameterWire(w, r, body)
					}
				}, wireProvider(endpoint, endpoint, "", map[string]bool{"temperature": true}), configure)
			require.Equal(t, http.StatusOK, reply.status, string(reply.body))
			require.Len(t, reply.receipts, 1)
			require.Equal(t, "0", gjson.GetBytes(reply.receipts[0].body, "temperature").Raw)
			require.True(t, gjson.ValidBytes(reply.body))
			for _, phase := range []int{responsecompat.ResponsesPhase, responsecompat.ParameterPhase} {
				arm, revision := "enforce", responseRevision
				if phase == responsecompat.ParameterPhase {
					arm, revision = "strict", parameterRevision
				}
				var gotArm, outcome, source string
				var gotRevision int64
				var applied, target bool
				err := db.QueryRowContext(t.Context(), "SELECT arm, revision, outcome, COALESCE(rejection_source,''), applied, target_parameter_error FROM gateway_compatibility_cohorts WHERE phase=$1 AND request_id=$2", phase, reply.id).
					Scan(&gotArm, &gotRevision, &outcome, &source, &applied, &target)
				if wantPending[phase] == 0 {
					require.ErrorIs(t, err, sql.ErrNoRows)
					continue
				}
				require.NoError(t, err)
				require.Equal(t, arm, gotArm)
				require.Equal(t, revision, gotRevision)
				require.Equal(t, "completed", outcome)
				require.Empty(t, source)
				require.Equal(t, phase == responsecompat.ResponsesPhase, applied)
				require.False(t, target)
			}
			t.Logf("request=%s D=%d D₂=%d; all enrolled rows completed", reply.id, wantPending[1], wantPending[2])
		})
	}

	t.Run("both_enabled_incomplete", func(t *testing.T) {
		reply := runParameterRequestWithProviders(t, db, endpoint, endpoint, parameterPayload(endpoint, model, false, `,"temperature":0`),
			func(w http.ResponseWriter, r *http.Request, body []byte) {
				assert.True(t, gjson.GetBytes(body, "stream").Bool())
				responsesSettlementStream(t, []string{settlementIncomplete}, false)(w, r)
			}, wireProvider(endpoint, endpoint, "", map[string]bool{"temperature": true}), configure)
		require.Equal(t, http.StatusOK, reply.status)
		require.Equal(t, "incomplete", gjson.GetBytes(reply.body, "status").Str)
		require.Len(t, reply.receipts, 1)
		for _, phase := range []int{responsecompat.ResponsesPhase, responsecompat.ParameterPhase} {
			var outcome string
			var applied bool
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT outcome, applied FROM gateway_compatibility_cohorts WHERE phase=$1 AND request_id=$2", phase, reply.id).Scan(&outcome, &applied))
			require.Equal(t, responsecompat.OutcomeIncomplete, outcome)
			require.Equal(t, phase == responsecompat.ResponsesPhase, applied)
		}
	})

	_, err := responses.Update(t.Context(), responsecompat.Config{Mode: responsecompat.Off, Revision: responseRevision})
	require.NoError(t, err)
	for _, pair := range []struct {
		inbound, outbound, schema string
	}{
		{requestpolicy.EndpointMessages, requestpolicy.EndpointMessages, `,"tools":[{"name":"lookup","input_schema":{"type":42}}]`},
		{requestpolicy.EndpointChat, requestpolicy.EndpointChat, `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":42}}}]`},
		{requestpolicy.EndpointResponses, requestpolicy.EndpointResponses, `,"tools":[{"type":"function","name":"lookup","parameters":{"type":42}}]`},
		{requestpolicy.EndpointResponses, requestpolicy.EndpointMessages, `,"tools":[{"type":"function","name":"lookup","parameters":{"type":42}}]`},
		{requestpolicy.EndpointChat, requestpolicy.EndpointMessages, `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":42}}}]`},
	} {
		t.Run("sources"+pair.inbound+"/to"+pair.outbound, func(t *testing.T) {
			for _, source := range []string{responsecompat.RejectionEntry, responsecompat.RejectionCandidate, responsecompat.RejectionUpstream} {
				t.Run(source, func(t *testing.T) {
					require.NoError(t, parameters.Refresh(t.Context()))
					fields := `,"temperature":0`
					if source == responsecompat.RejectionEntry {
						fields = pair.schema
					}
					reply := runParameterRequestWithProviders(t, db, pair.inbound, pair.outbound,
						parameterPayload(pair.inbound, model, false, fields), func(w http.ResponseWriter, _ *http.Request, _ []byte) {
							writeParameterFixtureRejection(t, w, parameterProbe{upCode: "invalid_parameter", wireField: "temperature", upMessage: "temperature is not supported on this model"})
						}, wireProvider(pair.inbound, pair.outbound, "", map[string]bool{"temperature": source != responsecompat.RejectionCandidate}), configure)
					require.Equal(t, http.StatusBadRequest, reply.status, string(reply.body))
					if source == responsecompat.RejectionUpstream {
						require.Len(t, reply.receipts, 1)
						require.Equal(t, "0", gjson.GetBytes(reply.receipts[0].body, "temperature").Raw)
					} else {
						require.Empty(t, reply.receipts)
					}
					assertParameterCohort(t, db, reply.id, parameterRevision, "strict", "parameter_400", source, pair.inbound, model)
				})
			}
		})
	}
	t.Run("recovered_upstream_400", func(t *testing.T) {
		require.NoError(t, parameters.Refresh(t.Context()))
		evidence := wireProvider(endpoint, endpoint, "", map[string]bool{"temperature": true, "tool_choice.named_function": true})
		providers := func(first *provider.Record) []*provider.Record {
			evidence(first)
			second := *first
			second.ID++
			second.Priority++
			second.Credentials = map[string]any{"api_key": "review-fixture-b", "base_url": first.Credentials["base_url"]}
			evidence(&second)
			return []*provider.Record{first, &second}
		}
		fields := `,"temperature":0,"tool_choice":{"type":"function","name":"lookup"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`
		reply := runParameterRequestWithProviders(t, db, endpoint, endpoint, parameterPayload(endpoint, model, false, fields),
			func(w http.ResponseWriter, r *http.Request, body []byte) {
				if r.Header.Get("Authorization") == "Bearer parameter-fixture-upstream" {
					writeParameterFixtureRejection(t, w, parameterProbe{upCode: "organization_deactivated", upMessage: "request rejected"})
					return
				}
				assert.Equal(t, "Bearer review-fixture-b", r.Header.Get("Authorization"))
				acceptParameterWire(w, r, body)
			}, providers, configure, func(input *gatewayHTTPFixtureInput) { input.MaxSwitches = 2 })
		require.Equal(t, http.StatusOK, reply.status, string(reply.body))
		require.Len(t, reply.receipts, 2)
		for _, packet := range reply.receipts {
			require.Equal(t, "0", gjson.GetBytes(packet.body, "temperature").Raw)
			require.Equal(t, "lookup", gjson.GetBytes(packet.body, "tool_choice.name").String())
		}
		assertParameterCohort(t, db, reply.id, parameterRevision, "strict", "completed", "", endpoint, model)
	})
	var pending int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM gateway_compatibility_cohorts WHERE outcome IS NULL").Scan(&pending))
	require.Zero(t, pending)
	t.Run("dedicated_antigravity", func(t *testing.T) {
		testParameterReviewAntigravity(t, db, parameters, cohorts, parameterRevision)
	})
	t.Run("terminal", func(t *testing.T) {
		testParameterReviewTerminals(t, db, parameters, configure, parameterRevision)
	})
	t.Run("stream_lifecycle", func(t *testing.T) {
		testParameterReviewStreamLifecycle(t, db, parameters, configure, parameterRevision)
	})
}

// parameterReviewStrictConfig scopes the review to the fixture group and model.
func parameterReviewStrictConfig(revision int64) requestpolicy.Config {
	config := requestpolicy.Config{Mode: requestpolicy.ModeStrict, Revision: revision}
	for _, endpoint := range []string{requestpolicy.EndpointResponses, requestpolicy.EndpointChat, requestpolicy.EndpointMessages} {
		config.Rules = append(config.Rules, requestpolicy.Rule{
			Entry: requestpolicy.Entry{EntryGroupID: parameterFixtureGroup, Endpoint: endpoint, RequestedModel: parameterWireClientModel},
			Mode:  requestpolicy.ModeStrict, StrictPercent: 100,
		})
	}
	return config
}

// testParameterReviewTerminals checks that the visible protocol result
// controls D2 even when the upstream and client HTTP status are 200.
func testParameterReviewTerminals(t *testing.T, db *sql.DB, parameters *requestpolicy.Service, configure func(*gatewayHTTPFixtureInput), revision int64) {
	t.Helper()
	const messages = requestpolicy.EndpointMessages
	const chat = requestpolicy.EndpointChat
	const responses = requestpolicy.EndpointResponses
	for _, tc := range []struct {
		endpoint, outcome, marker, frame string
		stream                           bool
	}{
		{messages, "incomplete", "max_tokens", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":1}}`, true},
		{messages, "refused", "refusal", `{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":1}}`, true},
		{chat, "incomplete", "length", `{"id":"chatcmpl-review","object":"chat.completion.chunk","model":"gpt-6-luna","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":"length"}]}`, true},
		{chat, "refused", "content_filter", `{"id":"chatcmpl-review","object":"chat.completion.chunk","model":"gpt-6-luna","choices":[{"index":0,"delta":{"refusal":"cannot answer"},"finish_reason":"content_filter"}]}`, true},
		{responses, "incomplete", "response.incomplete", settlementIncomplete, true},
		{responses, "refused", "cannot answer", `{"type":"response.completed","response":{"id":"resp_settle","object":"response","status":"completed","output":[{"id":"msg_review","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"cannot answer"}]}]}}`, true},
		{messages, "incomplete", "max_tokens", `{"id":"msg_review","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[{"type":"text","text":"partial"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`, false},
		{messages, "refused", "refusal", `{"id":"msg_review","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[{"type":"text","text":"cannot answer"}],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":1}}`, false},
		{chat, "incomplete", "length", `{"id":"chatcmpl-review","object":"chat.completion","model":"gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}]}`, false},
		{chat, "refused", "content_filter", `{"id":"chatcmpl-review","object":"chat.completion","model":"gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","refusal":"cannot answer"},"finish_reason":"content_filter"}]}`, false},
		{responses, "incomplete", "max_output_tokens", gjson.Get(settlementIncomplete, "response").Raw, false},
		{chat, "incomplete", "length", `{"id":"chatcmpl-large","object":"chat.completion","model":"gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.Repeat("x", 4<<20) + `"},"finish_reason":"length"}]}`, false},
		{messages, "incomplete", "max_tokens", `{"id":"msg_large","type":"message","role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("x", 4<<20) + `"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`, false},
		{responses, "refused", "cannot answer", `{"id":"resp_settle","object":"response","status":"completed","output":[{"id":"msg_review","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"cannot answer"}]}],` + settlementUsage + `}`, false},
	} {
		name := fmt.Sprintf("%s/%s/stream_%t", strings.TrimPrefix(tc.endpoint, "/v1/"), tc.outcome, tc.stream)
		if !tc.stream && len(tc.frame) > 4<<20 {
			name += "/large_json"
		}
		t.Run(name, func(t *testing.T) {
			require.NoError(t, parameters.Refresh(t.Context()))
			reply := runParameterRequest(t, db, tc.endpoint, tc.endpoint, parameterPayload(tc.endpoint, parameterWireClientModel, tc.stream, ""),
				func(w http.ResponseWriter, _ *http.Request, _ []byte) {
					if !tc.stream {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, tc.frame)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.endpoint == messages {
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_review\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
						_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"partial\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
						_, _ = io.WriteString(w, "event: message_delta\n")
					}
					_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.frame)
					switch tc.endpoint {
					case messages:
						_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					case chat:
						_, _ = io.WriteString(w, "data: [DONE]\n\n")
					}
				}, configure)
			require.Equal(t, http.StatusOK, reply.status, string(reply.body))
			require.Len(t, reply.receipts, 1)
			require.Contains(t, string(reply.body), tc.marker)
			assertParameterCohort(t, db, reply.id, revision, "strict", tc.outcome, "", tc.endpoint, parameterWireClientModel)
		})
	}
}

// testParameterReviewStreamLifecycle exercises ordinary terminals, in-band
// failure and real client cancellation on each native endpoint. Chat includes
// a refusal delta in its own wire frame and a later normal finish reason.
func testParameterReviewStreamLifecycle(t *testing.T, db *sql.DB, parameters *requestpolicy.Service, configure func(*gatewayHTTPFixtureInput), revision int64) {
	t.Helper()
	const messagesStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_review\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"partial\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	const chatBase = `{"id":"chatcmpl-review","object":"chat.completion.chunk","model":"gpt-6-luna","choices":[{"index":0,`
	const chatStart = "data: " + chatBase + `"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"
	const chatFinish = "data: " + chatBase + `"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
	const messagesFinish = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const responsesStart = "data: {\"type\":\"response.output_text.delta\",\"response_id\":\"resp_settle\",\"item_id\":\"msg_text\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n"
	for _, tc := range []struct {
		endpoint, start, finish, fail, failureMarker, failureOutcome string
	}{
		{
			requestpolicy.EndpointMessages, messagesStart, messagesFinish,
			"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"review failure\"}}\n\n", "event: error", responsecompat.OutcomeProtocol502,
		},
		{
			requestpolicy.EndpointChat, chatStart, chatFinish,
			"data: {\"error\":{\"type\":\"server_error\",\"message\":\"review failure\"}}\n\n", "review failure", responsecompat.OutcomeProtocol502,
		},
		{
			requestpolicy.EndpointResponses, responsesStart, "data: " + settlementCompleted + "\n\n",
			"data: " + settlementFailure + "\n\n", "response.failed", responsecompat.OutcomeOtherFailure,
		},
	} {
		for _, scenario := range []string{"completed", "failure", "disconnect"} {
			t.Run(strings.TrimPrefix(tc.endpoint, "/v1/")+"/"+scenario, func(t *testing.T) {
				require.NoError(t, parameters.Refresh(t.Context()))
				var before gin.HandlerFunc
				var read func(*http.Response, context.CancelFunc) ([]byte, error)
				var canceled chan struct{}
				if scenario == "disconnect" {
					canceled = make(chan struct{})
					before = func(c *gin.Context) {
						context.AfterFunc(c.Request.Context(), func() { close(canceled) })
					}
					read = func(response *http.Response, cancel context.CancelFunc) ([]byte, error) {
						reader := bufio.NewReader(response.Body)
						var first []byte
						for !bytes.Contains(first, []byte("\n\n")) {
							line, err := reader.ReadString('\n')
							if err != nil {
								return nil, err
							}
							first = append(first, line...)
						}
						cancel()
						return first, nil
					}
				}
				reply := runParameterRequestWithClient(t, db, tc.endpoint, tc.endpoint, parameterPayload(tc.endpoint, parameterWireClientModel, true, ""),
					func(w http.ResponseWriter, r *http.Request, _ []byte) {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, tc.start)
						if err := http.NewResponseController(w).Flush(); !assert.NoError(t, err) {
							return
						}
						if scenario == "disconnect" {
							// Wait for the gateway's socket context, so the simulated
							// terminal cannot win a race against client cancellation.
							select {
							case <-canceled:
							case <-r.Context().Done():
								return
							}
						}
						if scenario == "failure" {
							_, _ = io.WriteString(w, tc.fail)
							return
						}
						_, _ = io.WriteString(w, tc.finish)
					}, nil, before, read, configure)
				require.Equal(t, http.StatusOK, reply.status, string(reply.body))
				require.Contains(t, reply.contentType, "text/event-stream")
				require.Len(t, reply.receipts, 1)
				outcome := responsecompat.OutcomeCompleted
				switch scenario {
				case "failure":
					require.Contains(t, string(reply.body), tc.failureMarker)
					outcome = tc.failureOutcome
				case "disconnect":
					outcome = responsecompat.OutcomeClientGone
				}
				assertParameterCohort(t, db, reply.id, revision, "strict", outcome, "", tc.endpoint, parameterWireClientModel)
			})
		}
	}
	t.Run("chat_refusal_delta_then_stop", func(t *testing.T) {
		const endpoint = requestpolicy.EndpointChat
		require.NoError(t, parameters.Refresh(t.Context()))
		reply := runParameterRequest(t, db, endpoint, endpoint, parameterPayload(endpoint, parameterWireClientModel, true, ""),
			func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+chatBase+`"delta":{"ref`)
				if err := http.NewResponseController(w).Flush(); !assert.NoError(t, err) {
					return
				}
				_, _ = io.WriteString(w, `usal":"cannot answer"},"finish_reason":null}]}`+"\n\n"+chatFinish)
			}, configure)
		require.Equal(t, http.StatusOK, reply.status, string(reply.body))
		require.Contains(t, string(reply.body), `"refusal":"cannot answer"`)
		require.Contains(t, string(reply.body), `"finish_reason":"stop"`)
		require.Len(t, reply.receipts, 1)
		assertParameterCohort(t, db, reply.id, revision, "strict", responsecompat.OutcomeRefused, "", endpoint, parameterWireClientModel)
	})
}

// testParameterReviewAntigravity hits the registered dedicated entry through
// API key authentication. The first-priority Anthropic candidate is in the
// same group, so only platform forcing can account for the selected lane.
func testParameterReviewAntigravity(t *testing.T, db *sql.DB, parameters *requestpolicy.Service, cohorts *gatewaypg.CompatibilityCohorts, revision int64) {
	t.Helper()
	const inbound = requestpolicy.EndpointMessages
	const wireModel = "claude-sonnet-4-5"
	upstream := newResponsesCompatibilityUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// The completed upstream frame includes consumption so the adapter can
		// close its usage tail inside this single loopback HTTP exchange.
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"antigravity fixture\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":4,\"candidatesTokenCount\":2,\"totalTokenCount\":6}}}\n\n")
	})
	cfg := &config.Config{}
	cfg.Gateway.MaxRequestBodyBytes = 1 << 20
	cfg.Gateway.TextMaxRequestBodyBytes = 1 << 20
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	group := &routing.Group{
		ID: parameterFixtureGroup, Hydrated: true, Name: "review-fixture",
		Status: routing.StatusActive, RateMultiplier: 1, AllowedProtocols: []protocol.ProtocolID{protocol.ProtocolAnthropicMessages},
	}
	user := &identity.User{ID: parameterFixtureUser, Balance: 100, Status: "active"}
	key := &apikey.APIKey{
		ID: parameterFixtureKey, Key: parameterFixtureToken, Status: "active", UserID: user.ID,
		User: user, GroupID: &group.ID, Group: group, BillingMode: "balance", ModelMapping: map[string]string{parameterWireClientModel: wireModel},
	}
	keys := testkit.NewService(&openCodeHTTPKeys{key: key}, nil, nil, nil, nil, nil, cfg)
	t.Cleanup(keys.Stop)
	base := provider.Record{
		ID: parameterFixtureProvider, Platform: provider.PlatformAnthropic, Type: provider.ProviderTypeAPIKey,
		Name: "review-anthropic", Priority: 1, Status: "active", Schedulable: true, Concurrency: 1, GroupIDs: []int64{group.ID},
	}
	base.Credentials = map[string]any{"api_key": "review-anthropic", "base_url": upstream.url}
	base.Extra = map[string]any{provider.RequestParameterEvidenceExtraKey: []map[string]any{{
		"model": wireModel, "inbound": string(protocol.ProtocolAnthropicMessages), "outbound": string(protocol.ProtocolAnthropicMessages),
		"source": "review_fixture", "version": "1", "fields": map[string]bool{"temperature": true},
	}}}
	ag := base
	ag.ID, ag.Name, ag.Platform, ag.Type, ag.Priority = base.ID+1, "review-antigravity", provider.PlatformAntigravity, provider.ProviderTypeOAuth, 2
	ag.Credentials = map[string]any{"access_token": "review-antigravity", "project_id": "fixture-project"}
	ag.Extra = map[string]any{provider.RequestParameterEvidenceExtraKey: []map[string]any{{
		"model": wireModel, "inbound": string(protocol.ProtocolAnthropicMessages), "outbound": string(protocol.ProtocolGeminiGenerateContent),
		"source": "review_fixture", "version": "1", "fields": map[string]bool{"temperature": true},
	}}}
	pool := &mixedHTTPProviders{values: []gatewayprovider.ExecutionProvider{*gatewayprovider.NewExecutionProvider(&base), *gatewayprovider.NewExecutionProvider(&ag)}}
	transport := provideHTTPUpstream(cfg)
	t.Cleanup(transport.CloseIdleConnections)
	concurrency := scheduler.NewConcurrencyService(&fakeConcurrencyCache{})
	readers := &gatewayprovider.RuntimeReaders{}
	source, choices, messages := newGenericExecutionAndSelectionFixture(pool, &fakeGroupRepo{group: group}, nil, nil, cfg,
		nil, concurrency, nil, nil, transport, &provider.DeferredService{}, &provider.MessageCredentialSource{},
		nil, nil, nil, nil, readers, nil, nil, nil, nil)
	eligibility := newBillingEligibilityFixture(cfg)
	t.Cleanup(eligibility.Stop)
	funding := newFundingAdmissionFixture(eligibility, cfg)
	activity := &gatewayRequestActivity{Operations: lifecycle.NewOperations("parameter-review-antigravity")}
	shared := provideMessageHTTPBindings(source.Routes, nil, provideSchedulerSharedState(cfg, nil), funding,
		nil, nil, nil, nil, concurrency, cfg, choices, keys, nil)
	retry := &provideradapter.AntigravityRetry{
		Health:    &provider.AntigravityHealth{},
		BaseURL:   func(*provider.Record) string { return upstream.url },
		BodyLimit: func() int64 { return 512 << 10 }, SafeURL: func(path string) string { return path },
	}
	antigravity := provideAntigravityExecutor(provideAntigravityForward(pool, &provider.AntigravityTokenSource{}, retry, nil, transport, readers, nil, cfg, activity))
	usage := &gatewaytestkit.UsageLogStore{Inserted: true}
	recorder := gatewaytestkit.NewRecording(usage, &gatewaytestkit.SettlementStore{}, nil, false).Core(nil, false)
	runtime := provideMessageAttemptRuntime(source.Cooldown, nil, messages, antigravity, nil, funding, keys,
		GatewayCompletionRecorders{Forward: recorder}, shared, nil, nil, nil, cfg, nil, choices, nil, nil, nil)
	router := gin.New()
	finished := make(chan struct{}, 1)
	router.Use(func(c *gin.Context) {
		c.Next()
		// Response EOF can precede cohort finalization, so signal after the route has finished its database writes.
		finished <- struct{}{}
	})
	auth := provideAPIKeyAuth(keys, nil, cfg)
	RegisterGatewayRoutes(router, &routeTestHandlers{
		TextEnabled: true, OpenAIEnabled: true,
		MessagesHTTP: provideMessagesHTTP(shared, runtime, activity),
		OpenAITextHTTP: gatewayhttp.NewOpenAITextHandler(gatewayhttp.OpenAITextOptions{
			ParameterPolicy: parameters, CompatibilityCohorts: cohorts, MaxBodyBytes: cfg.Gateway.MaxRequestBodyBytes,
		}, nil, nil, nil),
	}, auth, keys, nil, nil, nil, cfg)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	post := func(fields string, status int) (string, []byte) {
		require.NoError(t, parameters.Refresh(t.Context()))
		ctx, cancel := context.WithTimeout(t.Context(), parameterFixtureTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/antigravity/v1/messages",
			strings.NewReader(parameterPayload(inbound, parameterWireClientModel, false, fields)))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+parameterFixtureToken)
		req.Header.Set("Content-Type", "application/json")
		res, err := server.Client().Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(res.Body)
		require.NoError(t, res.Body.Close())
		require.NoError(t, err)
		select {
		case <-finished:
		case <-ctx.Done():
			t.Fatal("dedicated Messages handler did not finish")
		}
		require.Equal(t, status, res.StatusCode, string(body))
		return res.Header.Get("X-TokenRouter-Request-ID"), body
	}
	id, body := post(`,"tools":[{"name":"lookup","input_schema":{"type":42}}]`, http.StatusBadRequest)
	require.Empty(t, upstream.snapshot())
	require.Contains(t, string(body), "tools[0].input_schema.type")
	assertParameterCohort(t, db, id, revision, "strict", "parameter_400", "entry", inbound, parameterWireClientModel)

	id, body = post(`,"temperature":0`, http.StatusOK)
	require.Contains(t, string(body), "antigravity fixture")
	packets := upstream.snapshot()
	require.Len(t, packets, 1)
	require.Equal(t, "/v1internal:streamGenerateContent", packets[0].path)
	require.Equal(t, wireModel, gjson.GetBytes(packets[0].body, "model").String())
	require.Equal(t, "0", gjson.GetBytes(packets[0].body, "request.generationConfig.temperature").Raw)
	assertParameterCohort(t, db, id, revision, "strict", "completed", "", inbound, parameterWireClientModel)
	require.Equal(t, 1, usage.Calls)
}
