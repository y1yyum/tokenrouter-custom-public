//go:build integration

package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// gateway_responses_compatibility_errors_scenario_integration_test.go compares
// the HTTP body and status with observations from openai_failover_error.go,
// responses_attempts.go, and compatibility_cohorts.go. Both sides use TCP so
// an unintended fallback or duplicate terminal appears as extra requests or
// invalid downstream framing.

// TestGatewayResponsesCompatibilityFinalErrors exercises classified failures
// after visible output, exhausted recovery, no candidate, and a real idle
// timeout. Stale downstream headers and an earlier failed attempt allow the
// test to distinguish relay of the final Retry-After from accidental reuse.
func TestGatewayResponsesCompatibilityFinalErrors(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough_%t", passthrough), func(t *testing.T) {
			for _, tc := range []struct {
				name, outcome, wantRetry                              string
				retry                                                 []string
				status                                                int
				preExecution, noCandidate, noBudget, switchToB, stall bool
			}{
				{name: "overload", outcome: responsecompat.OutcomeOverload503, status: 503, retry: []string{"45"}, wantRetry: "45"},
				{name: "overload_missing_header", outcome: responsecompat.OutcomeOverload503, status: 503},
				{name: "overload_invalid_header", outcome: responsecompat.OutcomeOverload503, status: 503, retry: []string{"invalid"}},
				{name: "overload_repeated_header", outcome: responsecompat.OutcomeOverload503, status: 503, retry: []string{"11", "12"}},
				{name: "protocol", outcome: responsecompat.OutcomeProtocol502, status: 502},
				{name: "idle_timeout", outcome: responsecompat.OutcomeTimeout504, status: 504, stall: true},
				{name: "no_candidate", outcome: responsecompat.OutcomeOverload503, status: 503, retry: []string{"45"}, wantRetry: "45", preExecution: true, noCandidate: true},
				{name: "budget_exhausted", outcome: responsecompat.OutcomeOverload503, status: 503, retry: []string{"45"}, wantRetry: "45", preExecution: true, noBudget: true},
				{name: "later_attempt_missing_header", outcome: responsecompat.OutcomeOverload503, status: 503, switchToB: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					policy := responsesCompatibilityPolicy(responsecompat.Enforce)
					stopped := make(chan struct{}, 1)
					a := func(w http.ResponseWriter, r *http.Request) {
						defer func() {
							select {
							case stopped <- struct{}{}:
							default:
							}
						}()
						for _, value := range tc.retry {
							w.Header().Add("Retry-After", value)
						}
						if tc.preExecution || tc.switchToB {
							if tc.switchToB {
								w.Header().Set("Retry-After", "45")
							}
							responsesCompatibilityPreExecutionRefusal(t)(w, r)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						frames := []string{replayCreated, replayText, replayFailed}
						if tc.status == http.StatusBadGateway {
							frames = []string{"[DONE]"}
						}
						if tc.stall {
							frames = frames[:2]
						}
						for _, frame := range frames {
							if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
								t.Errorf("write final error fixture: %v", err)
								return
							}
						}
						if tc.stall {
							if err := http.NewResponseController(w).Flush(); err != nil {
								t.Errorf("flush final error fixture: %v", err)
								return
							}
							<-r.Context().Done()
						}
					}
					b := responsesCompatibilityBadGateway(t)
					if tc.switchToB {
						rule := policy.Rules[0]
						rule.ProviderID = responsesCompatibilityB
						rule.UpstreamModel = responsesCompatibilityFallbackModel
						policy.Rules = append(policy.Rules, rule)
						b = func(w http.ResponseWriter, _ *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", replayText, replayFailed)
						}
					}
					fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
						passthrough: passthrough, rewrite: passthrough, policy: policy,
						switchDirectly: tc.preExecution || tc.switchToB, onlyPrimary: tc.noCandidate, disableSwitch: tc.noBudget,
						transport: openai.ResponsesTransportOptions{FirstOutputTimeout: 2 * time.Second, StreamInterval: 400 * time.Millisecond},
					}, a, b, func(c *gin.Context) { c.Header("Retry-After", "9") })
					reply := fixture.post(t, replayRequest)
					fixture.finish(t)
					require.Equal(t, tc.status, reply.status, "%s", reply.body)
					require.Contains(t, reply.header.Get("Content-Type"), "application/json")
					require.True(t, json.Valid(reply.body), "%s", reply.body)
					require.Equal(t, "upstream_error", gjson.GetBytes(reply.body, "error.type").String())
					require.NotContains(t, string(reply.body), "data: ")
					require.Equal(t, tc.wantRetry, reply.header.Get("Retry-After"))
					require.Len(t, fixture.a.snapshot(), 1)
					if tc.switchToB {
						require.Len(t, fixture.b.snapshot(), 1)
					} else {
						require.Empty(t, fixture.b.snapshot())
					}
					select {
					case <-stopped:
					case <-time.After(time.Second):
						t.Fatal("upstream exchange remained active after the terminal response")
					}
					assertResponsesCompatibilityErrorObservation(t, db, reply, tc.status, tc.outcome)
				})
			}
		})
	}
}

// TestGatewayResponsesCompatibilityAlreadyCommitted contrasts an unexpectedly
// committed synchronous writer with a genuine native stream. The former keeps
// its one original JSON body. The latter delivers the upstream failure as SSE
// without appending a duplicate terminal after the provider returns.
func TestGatewayResponsesCompatibilityAlreadyCommitted(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, passthrough := range []bool{false, true} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough_%t/native_%t", passthrough, native), func(t *testing.T) {
				a := func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", replayCreated, replayText)
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Errorf("flush first native frame: %v", err)
						return
					}
					_, _ = fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", replayFailed)
				}
				var middleware []gin.HandlerFunc
				payload := replayRequest
				if native {
					payload = strings.Replace(payload, `"stream":false`, `"stream":true`, 1)
				} else {
					middleware = append(middleware, func(c *gin.Context) {
						c.Data(http.StatusAccepted, "application/json", []byte(`{"original":true}`))
					})
				}
				fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
					passthrough: passthrough, rewrite: passthrough, policy: responsesCompatibilityPolicy(responsecompat.Enforce),
				}, a, responsesCompatibilityBadGateway(t), middleware...)
				reply := fixture.post(t, payload)
				fixture.finish(t)
				require.Len(t, fixture.a.snapshot(), 1)
				require.Empty(t, fixture.b.snapshot())
				if native {
					require.Equal(t, http.StatusOK, reply.status)
					require.Contains(t, reply.header.Get("Content-Type"), "text/event-stream")
					require.Contains(t, string(reply.body), `"delta":"working"`)
					require.Equal(t, 1, strings.Count(string(reply.body), `"type":"response.failed"`), "%s", reply.body)
					return
				}
				require.Equal(t, http.StatusAccepted, reply.status)
				require.Equal(t, `{"original":true}`, string(reply.body))
			})
		}
	}
}

// assertResponsesCompatibilityErrorObservation verifies one persisted Ops row
// and the one finalized cohort for the same logical request. Both status codes
// must match the synchronous error the client received over TCP.
func assertResponsesCompatibilityErrorObservation(t *testing.T, db *sql.DB, reply responsesCompatibilityReply, wantStatus int, wantOutcome string) {
	t.Helper()
	id := reply.header.Get("X-TokenRouter-Request-ID")
	require.NotEmpty(t, id)
	var count, status, upstreamStatus int
	var stream bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM ops_error_logs WHERE request_id=$1`, id).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT status_code, upstream_status_code, stream FROM ops_error_logs WHERE request_id=$1`, id).
		Scan(&status, &upstreamStatus, &stream))
	require.Equal(t, wantStatus, status)
	require.Equal(t, wantStatus, upstreamStatus)
	require.False(t, stream)
	var outcome string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT outcome FROM gateway_compatibility_cohorts WHERE phase=$1 AND request_id=$2`, responsecompat.ResponsesPhase, id).Scan(&outcome))
	require.Equal(t, wantOutcome, outcome)
}
