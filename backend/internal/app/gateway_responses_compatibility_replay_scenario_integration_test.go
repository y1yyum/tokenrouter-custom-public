//go:build integration

package app

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
)

// gateway_responses_compatibility_replay_scenario_integration_test.go counts actual
// upstream HTTP requests when the converted reader yields a failure.
// Both dispatches use the real Responses handler and capture bytes at A and B.
// One scenario flushes the rejection while keeping its body open, so closed
// replay and recovery after a complete read can be distinguished on the wire.

const (
	// These frames hold the response identity constant while varying work evidence.
	replayCreated = `{"type":"response.created","response":{"id":"resp_replay","status":"in_progress"}}`
	replayFailed  = `{"type":"response.failed","response":{"id":"resp_replay","status":"failed","error":{"code":"server_is_overloaded","message":"Overloaded","execution_started":false}}}`
	replayRequest = `{"model":"gpt-5.4","input":"return ok","max_tokens":20,"truncation":"auto","stream":false}`
	replayText    = `{"type":"response.output_text.delta","response_id":"resp_replay","item_id":"msg_replay","output_index":0,"content_index":0,"delta":"working"}`
)

// TestGatewayResponsesCompatibilityReplay observes candidate receipt counts
// with output, tool progress, metering, abrupt disconnect and a plain 503.
// Its positive control holds a flushed pre-execution failure open before EOF.
func TestGatewayResponsesCompatibilityReplay(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, route := range []struct {
		name string
		pass bool
	}{{"ordinary", false}, {"rewritable_passthrough", true}} {
		t.Run(route.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name         string
				frames       []string
				status       int
				switchToB    bool
				sameProvider bool
			}{
				{name: "text_then_overload", frames: []string{replayCreated, replayText, replayFailed}, status: 503},
				{name: "tool_then_overload", frames: []string{replayCreated, `{"type":"response.web_search_call.in_progress","response_id":"resp_replay","item_id":"search_replay","output_index":0}`, replayFailed}, status: 503},
				{name: "positive_metering_then_overload", frames: []string{replayCreated, `{"type":"response.in_progress","response":{"id":"resp_replay","status":"in_progress","usage":{"input_tokens":4,"output_tokens":0}}}`, replayFailed}, status: 503},
				{name: "plain_http_503", status: 503},
				{name: "overload_without_execution_proof", frames: []string{replayCreated, strings.Replace(replayFailed, `,"execution_started":false`, "", 1)}, status: 503},
				{name: "transport_result_unknown", status: 502},
				{name: "pre_execution_after_eof", frames: []string{replayCreated, replayFailed}, status: 200, switchToB: true},
				{name: "pre_execution_same_provider_then_lock", frames: []string{replayCreated, replayFailed}, status: 503, sameProvider: true},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					id := "resp_replay_b_" + route.name
					bStarted := make(chan struct{}, 1)
					var premature atomic.Bool
					var calls atomic.Int32
					a := func(w http.ResponseWriter, _ *http.Request) {
						switch scenario.name {
						case "transport_result_unknown":
							// TCP closes after the listener received the full request, with no headers returned.
							conn, _, err := http.NewResponseController(w).Hijack()
							if err != nil {
								t.Errorf("hijack A: %v", err)
								return
							}
							_ = conn.Close()
							return
						case "plain_http_503":
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = io.WriteString(w, `{"error":{"message":"Unavailable"}}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						frames := scenario.frames
						if scenario.sameProvider && calls.Add(1) > 1 {
							frames = []string{replayCreated, replayText, replayFailed}
						}
						for _, frame := range frames {
							if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
								t.Errorf("write A frame: %v", err)
								return
							}
						}
						if scenario.switchToB {
							// Flush the complete event but keep the HTTP message open. B
							// must stay idle until the strict reader reaches the real EOF.
							if err := http.NewResponseController(w).Flush(); err != nil {
								t.Errorf("flush A: %v", err)
								return
							}
							select {
							case <-bStarted:
								premature.Store(true)
							case <-time.After(100 * time.Millisecond):
							}
						}
					}
					success := responsesCompatibilitySuccess(t, id, responsesCompatibilityFallbackModel, "false")
					b := func(w http.ResponseWriter, r *http.Request) {
						select {
						case bStarted <- struct{}{}:
						default:
						}
						success(w, r)
					}
					fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
						passthrough: route.pass, rewrite: route.pass, policy: responsesCompatibilityPolicy(responsecompat.Enforce), switchDirectly: scenario.switchToB,
					}, a, b)
					reply := fixture.post(t, replayRequest)
					fixture.finish(t)
					require.Equal(t, scenario.status, reply.status, string(reply.body))
					requests := fixture.a.snapshot()
					expectedA := 1
					if scenario.sameProvider {
						expectedA = 2
					}
					require.Len(t, requests, expectedA)
					for index := range requests {
						assertResponsesCompatibilityWireRequest(t, requests[index:index+1], responsesCompatibilityA, responsesCompatibilityLuna, "true", "text/event-stream", responsesCompatibilityPath, route.pass)
					}
					require.False(t, premature.Load())
					if scenario.switchToB {
						assertResponsesCompatibilityRequest(t, fixture.b.snapshot(), responsesCompatibilityB, responsesCompatibilityFallbackModel, "false", route.pass)
						assertResponsesCompatibilityReply(t, reply, id, false)
					} else {
						require.Empty(t, fixture.b.snapshot())
						require.True(t, gjson.ValidBytes(reply.body), string(reply.body))
						require.True(t, gjson.GetBytes(reply.body, "error").IsObject())
						require.NotContains(t, string(reply.body), "data:")
					}
				})
			}
		})
	}
}

// TestGatewayResponsesCompatibilityReplayFieldRepair verifies that one precise
// HTTP validation rejection changes the next A request in each dispatch.
// The same rejection marked as already executed produces no second receipt.
func TestGatewayResponsesCompatibilityReplayFieldRepair(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, route := range []struct {
		name string
		pass bool
	}{{"ordinary", false}, {"rewritable_passthrough", true}} {
		t.Run(route.name, func(t *testing.T) {
			for _, alreadyExecuted := range []bool{false, true} {
				t.Run(fmt.Sprintf("executed_%t", alreadyExecuted), func(t *testing.T) {
					var calls atomic.Int32
					const id = "resp_replay_repaired"
					success := responsesCompatibilityCompletedStream(t, id, responsesCompatibilityLuna)
					a := func(w http.ResponseWriter, r *http.Request) {
						if calls.Add(1) > 1 {
							success(w, r)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","code":"unknown_parameter","param":"truncation","message":"Unknown parameter: truncation","execution_started":%t}}`, alreadyExecuted)
					}
					fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
						passthrough: route.pass, rewrite: route.pass, policy: responsesCompatibilityPolicy(responsecompat.Enforce),
					}, a, responsesCompatibilityBadGateway(t))
					reply := fixture.post(t, replayRequest)
					fixture.finish(t)
					requests := fixture.a.snapshot()
					require.Empty(t, fixture.b.snapshot())
					if alreadyExecuted {
						require.Len(t, requests, 1)
						require.NotEqual(t, http.StatusOK, reply.status)
						require.True(t, gjson.ValidBytes(reply.body), string(reply.body))
						require.True(t, gjson.GetBytes(reply.body, "error").IsObject())
					} else {
						require.Len(t, requests, 2, string(reply.body))
						require.True(t, gjson.GetBytes(requests[0].body, "truncation").Exists())
						require.False(t, gjson.GetBytes(requests[1].body, "truncation").Exists())
						require.Equal(t, "true", gjson.GetBytes(requests[1].body, "stream").Raw)
						assertResponsesCompatibilityReply(t, reply, id, false)
					}
				})
			}
		})
	}
}
