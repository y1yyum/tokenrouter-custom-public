//go:build integration

package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
)

// gateway_parameter_wire_scenario_integration_test.go drives all three HTTP
// handlers and checks the final bytes received by native and bridged mock
// providers. It covers the requestpolicy outbound audit, Responses readers,
// Anthropic conversion, and ordinary and passthrough repairs.

const (
	parameterWireClientModel = "gpt-6-luna"   // original alias for all strict entry rules
	parameterAdaptiveModel   = "MiniMax-M2.7" // Messages model with a registered adaptive encoder
)

// TestGatewayParameterWire drives the final policy through authenticated HTTP.
func TestGatewayParameterWire(t *testing.T) {
	db := qualityTestDatabase(t)
	policy := strictWirePolicy(t, db)
	t.Run("fields", func(t *testing.T) { testParameterWireFields(t, db, policy) })
	t.Run("cache", func(t *testing.T) { testParameterWireCache(t, db, policy) })
	t.Run("tool_limit", func(t *testing.T) { testParameterWireToolLimit(t, db, policy) })
	t.Run("repair", func(t *testing.T) { testParameterWireRepair(t, db, policy) })
}

// testParameterWireFields checks native and bridged preservation and local
// schema refusals before the mock receives a request.
func testParameterWireFields(t *testing.T, db *sql.DB, policy func(*gatewayHTTPFixtureInput)) {
	for _, tc := range []struct {
		name, inbound, outbound, fields string
		model                           string
		support                         map[string]bool
		wire                            map[string]string
		missing                         []string
		fieldError                      string
	}{
		{
			name: "messages_user_budget", inbound: requestpolicy.EndpointMessages, outbound: requestpolicy.EndpointMessages,
			fields:  `,"thinking":{"type":"enabled","budget_tokens":1024}`,
			support: map[string]bool{"thinking.budget_tokens": true},
			wire:    map[string]string{"thinking.type": `"enabled"`, "thinking.budget_tokens": "1024"},
		},
		{
			name: "messages_user_budget_on_adaptive", inbound: requestpolicy.EndpointMessages, outbound: requestpolicy.EndpointMessages,
			model: parameterAdaptiveModel, fields: `,"thinking":{"type":"enabled","budget_tokens":1024}`,
			support:    map[string]bool{"thinking.budget_tokens": false},
			fieldError: "thinking.budget_tokens",
		},
		{
			name: "chat_derived_budget_on_budget_route", inbound: requestpolicy.EndpointChat, outbound: requestpolicy.EndpointMessages,
			fields: `,"reasoning_effort":"medium"`, wire: map[string]string{"thinking.type": `"enabled"`, "thinking.budget_tokens": "4096", "output_config.effort": `"medium"`},
		},
		{
			name: "chat_derived_budget_on_adaptive", inbound: requestpolicy.EndpointChat, outbound: requestpolicy.EndpointMessages,
			model: parameterAdaptiveModel, fields: `,"reasoning_effort":"medium"`,
			wire: map[string]string{"thinking.type": `"adaptive"`, "output_config.effort": `"medium"`}, missing: []string{"thinking.budget_tokens"},
		},
		{
			name: "chat_named_tool_and_zero", inbound: requestpolicy.EndpointChat, outbound: requestpolicy.EndpointMessages,
			fields:  `,"temperature":0,"tool_choice":{"type":"function","function":{"name":"lookup"}},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`,
			support: map[string]bool{"temperature": true, "tool_choice.named_function": true},
			wire:    map[string]string{"temperature": "0", "tool_choice.type": `"tool"`, "tool_choice.name": `"lookup"`, "tools.0.name": `"lookup"`},
		},
		{
			name: "responses_named_tool_no_temperature", inbound: requestpolicy.EndpointResponses, outbound: requestpolicy.EndpointMessages,
			fields:  `,"tool_choice":{"type":"function","name":"lookup"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`,
			support: map[string]bool{"tool_choice.named_function": true},
			wire:    map[string]string{"tool_choice.type": `"tool"`, "tool_choice.name": `"lookup"`, "tools.0.name": `"lookup"`}, missing: []string{"temperature"},
		},
		{
			name: "messages_function_schema", inbound: requestpolicy.EndpointMessages, outbound: requestpolicy.EndpointMessages,
			fields:     `,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"x":{"type":13}}}}]`,
			fieldError: `tools[0].input_schema.properties["x"].type`,
		},
		{
			name: "chat_function_schema", inbound: requestpolicy.EndpointChat, outbound: requestpolicy.EndpointMessages,
			fields:     `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"x":{"type":13}}}}}]`,
			fieldError: `tools[0].function.parameters.properties["x"].type`,
		},
		{
			name: "responses_output_schema", inbound: requestpolicy.EndpointResponses, outbound: requestpolicy.EndpointResponses,
			fields:     `,"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"x":{"type":13}}}}}`,
			fieldError: `text.format.schema.properties["x"].type`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := parameterPayload(tc.inbound, parameterWireClientModel, false, tc.fields)
			reply := runParameterRequestWithProviders(t, db, tc.inbound, tc.outbound, payload, acceptParameterWire,
				wireProvider(tc.inbound, tc.outbound, tc.model, tc.support), policy)
			if tc.fieldError != "" {
				require.Equal(t, http.StatusBadRequest, reply.status, "%s", reply.body)
				require.Contains(t, gjson.GetBytes(reply.body, "error.message").String(), tc.fieldError)
				require.Empty(t, reply.receipts)
				return
			}
			require.Equal(t, http.StatusOK, reply.status, "%s", reply.body)
			require.Len(t, reply.receipts, 1)
			for path, raw := range tc.wire {
				require.Equal(t, raw, gjson.GetBytes(reply.receipts[0].body, path).Raw, path)
			}
			for _, path := range tc.missing {
				require.False(t, gjson.GetBytes(reply.receipts[0].body, path).Exists(), path)
			}
		})
	}
}

// testParameterWireCache checks one complete cache decision for eight
// blocks. A denied final TTL produces zero receipts, and an upstream unknown
// field after a verified send produces exactly one receipt without deletion.
func testParameterWireCache(t *testing.T, db *sql.DB, policy func(*gatewayHTTPFixtureInput)) {
	const inbound = requestpolicy.EndpointResponses
	for _, outbound := range []string{requestpolicy.EndpointResponses, requestpolicy.EndpointMessages} {
		for _, verdict := range []string{"keep", "reject_at_selection", "reject_at_upstream"} {
			t.Run(outbound+"/"+verdict, func(t *testing.T) {
				var content []string
				for i := range 8 {
					ttl := "5m"
					if i == 7 {
						ttl = "1h"
					}
					content = append(content, fmt.Sprintf(`{"type":"input_text","text":"block-%d","cache_control":{"type":"ephemeral","ttl":%q}}`, i, ttl))
				}
				payload := fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":[%s]}]}`, parameterWireClientModel, strings.Join(content, ","))
				support := map[string]bool{
					"cache_control.input.content.user.input_text.ephemeral.5m": true,
					"cache_control.input.content.user.input_text.ephemeral.1h": verdict != "reject_at_selection",
				}
				respond := acceptParameterWire
				if verdict == "reject_at_upstream" {
					respond = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
						writeParameterFixtureRejection(t, w, parameterProbe{upCode: "unknown_parameter", wireField: "input[0].content[7].cache_control", upMessage: "Unknown parameter: input[0].content[7].cache_control"})
					}
				}
				reply := runParameterRequestWithProviders(t, db, inbound, outbound, payload, respond, wireProvider(inbound, outbound, "", support), policy)
				if verdict == "reject_at_selection" {
					require.Equal(t, http.StatusBadRequest, reply.status, "%s", reply.body)
					require.Contains(t, string(reply.body), "input.0.content.7.cache_control")
					require.Empty(t, reply.receipts)
					return
				}
				require.Len(t, reply.receipts, 1)
				base := "input.0.content"
				if outbound == requestpolicy.EndpointMessages {
					base = "messages.0.content"
				}
				blocks := gjson.GetBytes(reply.receipts[0].body, base).Array()
				require.Len(t, blocks, 8)
				for i, block := range blocks {
					require.Equal(t, fmt.Sprintf("block-%d", i), block.Get("text").String())
					want := "5m"
					if i == 7 {
						want = "1h"
					}
					require.Equal(t, want, block.Get("cache_control.ttl").String())
				}
				if verdict == "keep" {
					require.Equal(t, http.StatusOK, reply.status, "%s", reply.body)
				} else {
					require.Equal(t, http.StatusBadRequest, reply.status, "%s", reply.body)
				}
			})
		}
	}
	t.Run("messages_bridge_cannot_encode_scope", func(t *testing.T) {
		const outbound = requestpolicy.EndpointMessages
		payload := fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":[{"type":"input_text","text":"scoped","cache_control":{"type":"ephemeral","ttl":"5m","scope":"fixture"}}]}]}`, parameterWireClientModel)
		reply := runParameterRequestWithProviders(t, db, inbound, outbound, payload, acceptParameterWire,
			wireProvider(inbound, outbound, "", map[string]bool{"cache_control.input.content.user.input_text.ephemeral.5m.fixture": true}), policy)
		require.Equal(t, http.StatusBadRequest, reply.status, "%s", reply.body)
		require.Contains(t, string(reply.body), "input.0.content.0.cache_control")
		require.Empty(t, reply.receipts)
	})
}

// testParameterWireRepair returns an execution-free error for a supported
// syntax repair. Two candidates spend six tickets together while each sees a
// fresh initial body and keeps its own history. An explicit output cap stays
// on the only packet sent for a destructive repair refusal.
func testParameterWireRepair(t *testing.T, db *sql.DB, policy func(*gatewayHTTPFixtureInput)) {
	const endpoint = requestpolicy.EndpointResponses
	for _, firstPassthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("first_passthrough_%t", firstPassthrough), func(t *testing.T) {
			var input []string
			for i := range 8 {
				input = append(input, fmt.Sprintf(`{"type":"message","role":"user","content":"block-%d","prompt_cache_breakpoint":true}`, i))
			}
			payload := fmt.Sprintf(`{"model":%q,"input":[%s]}`, parameterWireClientModel, strings.Join(input, ","))
			providers := func(first *provider.Record) []*provider.Record {
				second := *first
				second.ID++
				second.Priority++
				second.Credentials = map[string]any{
					"api_key": "second-wire-fixture", "base_url": first.Credentials["base_url"],
					"model_mapping": first.Credentials["model_mapping"],
				}
				first.Extra = map[string]any{"openai_passthrough": firstPassthrough, "openai_passthrough_allow_rewrite": firstPassthrough}
				second.Extra = map[string]any{"openai_passthrough": !firstPassthrough, "openai_passthrough_allow_rewrite": !firstPassthrough}
				return []*provider.Record{first, &second}
			}
			respond := func(w http.ResponseWriter, r *http.Request, body []byte) {
				blocks := gjson.GetBytes(body, "input").Array()
				for i, block := range blocks {
					if r.Header.Get("Authorization") == "Bearer parameter-fixture-upstream" && i == 4 {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"fixture switch provider"}}`))
						return
					}
					if block.Get("prompt_cache_breakpoint").Exists() {
						field := fmt.Sprintf("input[%d].prompt_cache_breakpoint", i)
						writeParameterFixtureRejection(t, w, parameterProbe{upCode: "invalid_parameter", wireField: field, upMessage: field + " is not supported on this model"})
						return
					}
				}
				acceptParameterWire(w, r, body)
			}
			allowSwitch := func(input *gatewayHTTPFixtureInput) { input.MaxSwitches = 2 }
			reply := runParameterRequestWithProviders(t, db, endpoint, endpoint, payload, respond, providers, policy, allowSwitch)
			require.Equal(t, http.StatusBadRequest, reply.status, "%s", reply.body)
			require.Len(t, reply.receipts, 8)
			for i, sent := range reply.receipts {
				cleared := i
				if i >= 5 {
					cleared = i - 5
				}
				for j, block := range gjson.GetBytes(sent.body, "input").Array() {
					require.Equal(t, j >= cleared, block.Get("prompt_cache_breakpoint").Exists(), "packet %d block %d", i, j)
				}
			}
		})
		t.Run(fmt.Sprintf("reject_output_removal_passthrough_%t", firstPassthrough), func(t *testing.T) {
			providers := func(target *provider.Record) []*provider.Record {
				target.Extra = map[string]any{"openai_passthrough": firstPassthrough, "openai_passthrough_allow_rewrite": firstPassthrough}
				return []*provider.Record{target}
			}
			respond := func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				writeParameterFixtureRejection(t, w, parameterProbe{upCode: "unknown_parameter", wireField: "max_output_tokens", upMessage: "Unknown parameter: max_output_tokens"})
			}
			reply := runParameterRequestWithProviders(t, db, endpoint, endpoint, parameterPayload(endpoint, parameterWireClientModel, false, `,"max_output_tokens":1234`), respond, providers, policy)
			require.Equal(t, http.StatusBadRequest, reply.status, "%s", reply.body)
			require.Len(t, reply.receipts, 1)
			require.EqualValues(t, 1234, gjson.GetBytes(reply.receipts[0].body, "max_output_tokens").Int())
		})
	}
	t.Run("bridged_user_budget_rejected_once", func(t *testing.T) {
		const outbound = requestpolicy.EndpointMessages
		respond := func(w http.ResponseWriter, _ *http.Request, _ []byte) {
			writeParameterFixtureRejection(t, w, parameterProbe{wireField: "thinking.budget_tokens", upMessage: "thinking budget_tokens must be >= 1024"})
		}
		payload := parameterPayload(endpoint, parameterWireClientModel, false, `,"thinking":{"type":"enabled","budget_tokens":512}`)
		reply := runParameterRequestWithProviders(t, db, endpoint, outbound, payload, respond,
			wireProvider(endpoint, outbound, "", map[string]bool{"thinking.budget_tokens": true}), policy)
		require.Equal(t, http.StatusBadRequest, reply.status, "%s", reply.body)
		require.Len(t, reply.receipts, 1)
		require.Equal(t, "512", gjson.GetBytes(reply.receipts[0].body, "thinking.budget_tokens").Raw)
	})
}

// testParameterWireToolLimit checks both Responses runners and the
// native SSE response envelope using actual listener receipts.
func testParameterWireToolLimit(t *testing.T, db *sql.DB, policy func(*gatewayHTTPFixtureInput)) {
	const endpoint = requestpolicy.EndpointResponses
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			name, fields, sent, echoed string
			stream, supports           bool
		}{
			{"empty_json", `,"max_tool_calls":2`, "", "2", false, false},
			{"empty_sse", `,"max_tool_calls":2`, "", "2", true, false},
			{"function", `,"max_tool_calls":2,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`, "", "2", false, false},
			{"unknown_tool", `,"max_tool_calls":2,"tools":[{"type":"unverified_tool"}]`, "2", "", false, true},
			{"session_prompt", `,"max_tool_calls":2,"prompt":{"id":"pmpt_fixture"}`, "2", "", false, true},
			{"explicit_zero", `,"max_tool_calls":0`, "0", "", false, true},
			{"omitted", "", "", "", false, true},
		} {
			t.Run(fmt.Sprintf("passthrough_%t/%s", passthrough, tc.name), func(t *testing.T) {
				base := wireProvider(endpoint, endpoint, "", map[string]bool{
					"max_tool_calls": tc.supports, "max_tool_calls.unverified_tool": tc.supports, "max_tool_calls.inherited": tc.supports,
				})
				providers := func(target *provider.Record) []*provider.Record {
					targets := base(target)
					target.Extra["openai_passthrough"] = passthrough
					target.Extra["openai_passthrough_allow_rewrite"] = passthrough
					return targets
				}
				payload := parameterPayload(endpoint, parameterWireClientModel, tc.stream, tc.fields)
				reply := runParameterRequestWithProviders(t, db, endpoint, endpoint, payload, acceptParameterWire, providers, policy)
				require.Equal(t, http.StatusOK, reply.status, "%s", reply.body)
				require.Len(t, reply.receipts, 1)
				require.Equal(t, tc.sent, gjson.GetBytes(reply.receipts[0].body, "max_tool_calls").Raw)
				if tc.stream {
					require.Contains(t, string(reply.body), `"max_tool_calls":`+tc.echoed)
					require.Contains(t, string(reply.body), "response.completed")
				} else {
					require.Equal(t, tc.echoed, gjson.GetBytes(reply.body, "max_tool_calls").Raw)
				}
			})
		}
	}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("messages_bridge_local_function_stream_%t", stream), func(t *testing.T) {
			const outbound = requestpolicy.EndpointMessages
			payload := parameterPayload(endpoint, parameterWireClientModel, stream,
				`,"max_tool_calls":3,"tool_choice":{"type":"function","name":"lookup"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`)
			reply := runParameterRequestWithProviders(t, db, endpoint, outbound, payload, acceptParameterWire,
				wireProvider(endpoint, outbound, "", map[string]bool{"tool_choice.named_function": true}), policy)
			require.Equal(t, http.StatusOK, reply.status, "%s", reply.body)
			require.Len(t, reply.receipts, 1)
			require.False(t, gjson.GetBytes(reply.receipts[0].body, "max_tool_calls").Exists())
			require.Equal(t, "lookup", gjson.GetBytes(reply.receipts[0].body, "tool_choice.name").String())
			if stream {
				require.Contains(t, string(reply.body), `"max_tool_calls":3`)
				require.Contains(t, string(reply.body), "response.completed")
			} else {
				require.Equal(t, "3", gjson.GetBytes(reply.body, "max_tool_calls").Raw)
			}
		})
	}
}

// strictWirePolicy publishes one strict rule for each public protocol and
// refreshes the local snapshot for each fixture's authenticated request.
func strictWirePolicy(t *testing.T, db *sql.DB) func(*gatewayHTTPFixtureInput) {
	t.Helper()
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	service := requestpolicy.New(provideSettingsStore(client))
	config := requestpolicy.Config{Mode: requestpolicy.ModeStrict}
	for _, endpoint := range []string{requestpolicy.EndpointMessages, requestpolicy.EndpointChat, requestpolicy.EndpointResponses} {
		config.Rules = append(config.Rules, requestpolicy.Rule{
			Entry: requestpolicy.Entry{EntryGroupID: parameterFixtureGroup, Endpoint: endpoint, RequestedModel: parameterWireClientModel}, Mode: requestpolicy.ModeStrict, StrictPercent: 100,
		})
	}
	_, err := service.Update(t.Context(), config)
	require.NoError(t, err)
	return func(input *gatewayHTTPFixtureInput) {
		require.NoError(t, service.Refresh(t.Context()))
		input.ParameterPolicy = service
	}
}

// wireProvider attaches fixture-only evidence to the mapped model and exact
// protocol pair. Empty model keeps the fixture's default mapping.
func wireProvider(inbound, outbound, model string, fields map[string]bool) func(*provider.Record) []*provider.Record {
	return func(target *provider.Record) []*provider.Record {
		if model == "" {
			model = parameterWireModel(outbound, parameterWireClientModel)
		}
		target.Credentials["model_mapping"] = map[string]any{parameterWireClientModel: model}
		if target.Extra == nil {
			target.Extra = make(map[string]any)
		}
		protocols := map[string]protocol.ProtocolID{
			requestpolicy.EndpointMessages: protocol.ProtocolAnthropicMessages, requestpolicy.EndpointChat: protocol.ProtocolOpenAIChatCompletions,
			requestpolicy.EndpointResponses: protocol.ProtocolOpenAIResponses,
		}
		target.Extra[provider.RequestParameterEvidenceExtraKey] = []map[string]any{{
			"model": model, "inbound": string(protocols[inbound]), "outbound": string(protocols[outbound]),
			"source": "wire_fixture", "version": "1", "fields": fields,
		}}
		return []*provider.Record{target}
	}
}

// acceptParameterWire returns the protocol-specific success for the final wire request.
func acceptParameterWire(w http.ResponseWriter, r *http.Request, _ []byte) {
	mixedUpstreamResponse(w, r, false)
}
