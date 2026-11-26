//go:build integration

package app

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// TestGatewayResponsesCompatibilityTiming exercises both Responses POST paths
// over TCP. A stalled upstream yields a JSON 504, closes its connection,
// and receives one POST. An expired pre-send attempt has neither a handoff
// nor a received POST. The fallback receives no request in either case.
func TestGatewayResponsesCompatibilityTiming(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, route := range []struct {
		name        string
		passthrough bool
	}{
		{name: "ordinary"},
		{name: "rewrite_passthrough", passthrough: true},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name     string
				first    time.Duration
				idle     time.Duration
				total    time.Duration
				headers  bool
				output   bool
				ping     bool
				preSend  bool
				overload bool
				message  string
			}{
				{name: "expired_before_send", first: time.Nanosecond, idle: time.Second, total: 2 * time.Second, preSend: true, message: "Upstream Responses timed out waiting for response headers"},
				{name: "headers", first: 550 * time.Millisecond, idle: time.Second, total: 2 * time.Second, message: "Upstream Responses timed out waiting for response headers"},
				{name: "heartbeats", first: 550 * time.Millisecond, idle: 400 * time.Millisecond, total: 2 * time.Second, headers: true, ping: true, message: "Upstream Responses timed out waiting for first output"},
				{name: "idle_after_output", first: 850 * time.Millisecond, idle: 350 * time.Millisecond, total: 2 * time.Second, headers: true, output: true, message: "Upstream Responses timed out between upstream reads"},
				{name: "total_despite_output_and_ping", first: 500 * time.Millisecond, idle: 400 * time.Millisecond, total: 850 * time.Millisecond, headers: true, output: true, ping: true, message: "Upstream Responses request deadline exceeded"},
				{name: "confirmed_overload_then_idle", first: 850 * time.Millisecond, idle: 350 * time.Millisecond, total: 2 * time.Second, headers: true, overload: true},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					stopped := make(chan struct{})
					var dispatched atomic.Int32
					a := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer close(stopped)
						controller := http.NewResponseController(w)
						if scenario.headers {
							w.Header().Set("Content-Type", "text/event-stream")
							w.WriteHeader(http.StatusOK)
							_, _ = io.WriteString(w, ": initial\n\n")
							if scenario.output {
								_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"response_id\":\"resp_timing\",\"item_id\":\"msg_timing\",\"output_index\":0,\"content_index\":0,\"delta\":\"ok\"}\n\n")
							}
							if scenario.overload {
								_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_timing\",\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Overloaded\"}}}\n\n")
							}
							if err := controller.Flush(); err != nil {
								t.Errorf("flush initial upstream frames: %v", err)
								return
							}
						}
						if !scenario.ping {
							<-r.Context().Done()
							return
						}
						ticker := time.NewTicker(20 * time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-ticker.C:
								// The timeout can close the connection on this tick. Stop
								// the heartbeat if the socket fails before cancellation is visible.
								if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
									return
								}
								if err := controller.Flush(); err != nil {
									return
								}
							case <-r.Context().Done():
								return
							}
						}
					})
					b := responsesCompatibilitySuccess(t, "resp_timing_b", responsesCompatibilityFallbackModel, "false")
					fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
						passthrough: route.passthrough, rewrite: true, policy: responsesCompatibilityPolicy(responsecompat.Enforce),
						transport: openai.ResponsesTransportOptions{
							FirstOutputTimeout: scenario.first, StreamInterval: scenario.idle,
							OnDispatch: func() { dispatched.Add(1) },
						},
						total: scenario.total,
					}, a, b)
					reply := fixture.post(t, `{"model":"gpt-5.4","input":"return ok","max_tokens":20,"stream":false}`)
					if scenario.overload {
						require.Equal(t, http.StatusServiceUnavailable, reply.status, string(reply.body))
					} else {
						require.Equal(t, http.StatusGatewayTimeout, reply.status, string(reply.body))
						require.Equal(t, scenario.message, gjson.GetBytes(reply.body, "error.message").String())
					}
					require.True(t, strings.HasPrefix(reply.header.Get("Content-Type"), "application/json"))
					require.NotContains(t, string(reply.body), "data:")
					require.Empty(t, fixture.b.snapshot())
					if scenario.preSend {
						require.Empty(t, fixture.a.snapshot())
						require.Zero(t, dispatched.Load())
						return
					}
					require.Len(t, fixture.a.snapshot(), 1)
					require.EqualValues(t, 1, dispatched.Load())
					select {
					case <-stopped:
					case <-time.After(time.Second):
						t.Fatal("upstream remained active after timeout")
					}
				})
			}
		})
	}
}
