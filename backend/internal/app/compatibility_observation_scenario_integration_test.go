//go:build integration

package app

import (
	"fmt"
	"net/http"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/gateway/errorpolicy"
	gatewaypg "github.com/TokenFlux/TokenRouter/internal/gateway/postgres"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
)

// compatibility_observation_scenario_integration_test.go checks the control
// arm's final semantic status with a real gateway and a real presentation rule.
// A failure from an earlier candidate cannot overwrite a later success.

// TestResponsesObservationFinalStatuses compares two HTTP providers and the
// persisted cohort after normal failover. The control keeps the legacy HTTP
// response; only its trial outcome uses the final upstream failure evidence.
func TestResponsesObservationFinalStatuses(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	entClient := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	rules := errorpolicy.NewErrorPassthroughService(gatewaypg.NewErrorPassthroughRepository(entClient), nil)
	_, err := rules.Create(t.Context(), &errorpolicy.ErrorPassthroughRule{
		Name: "preserve upstream timeout", Enabled: true, MatchMode: errorpolicy.MatchModeAny,
		ErrorCodes: []int{503, 504}, PassthroughCode: true, PassthroughBody: true, Platforms: []string{"openai"},
	})
	require.NoError(t, err)
	for _, pass := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			status  int
			msg     string
			want    string
			recover bool
		}{
			{"overload", 503, "Server is overloaded", responsecompat.OutcomeOverload503, false},
			{"timeout", 504, "Upstream gateway timed out", responsecompat.OutcomeTimeout504, false},
			{"unavailable", 503, "Service temporarily unavailable", responsecompat.OutcomeOtherFailure, false},
			{"recovered", 504, "Upstream gateway timed out", responsecompat.OutcomeCompleted, true},
		} {
			t.Run(fmt.Sprintf("%s/pass=%t", tc.name, pass), func(t *testing.T) {
				up := func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"server_error"}}`, tc.msg)
				}
				var fallback http.HandlerFunc = up
				status := tc.status
				if tc.recover {
					fallback = responsesCompatibilitySuccess(t, "resp_recovered", responsesCompatibilityFallbackModel, "false")
					status = http.StatusOK
				}
				f := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
					passthrough: pass, rewrite: true, switchDirectly: true, policy: responsesCompatibilityPolicy(responsecompat.Observe), errorRules: rules,
				}, up, fallback)
				reply := f.post(t, `{"model":"gpt-5.4","input":"hello","stream":false}`)
				f.finish(t)
				require.Equal(t, status, reply.status, string(reply.body))
				var outcome string
				require.NoError(t, db.QueryRowContext(t.Context(), "SELECT outcome FROM gateway_compatibility_cohorts WHERE phase=1 AND request_id=$1", reply.header.Get("X-TokenRouter-Request-ID")).Scan(&outcome))
				require.Equal(t, tc.want, outcome)
			})
		}
	}
}

// TestResponsesMixedOutputCohort delivers the same JSON in both rollout arms.
// A refusal accompanied by usable text retains the response status; a refusal
// alone is counted separately. The control arm's JSON is not rewritten to
// accommodate the metric.
func TestResponsesMixedOutputCohort(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, pass := range []bool{false, true} {
		for _, mode := range []responsecompat.Mode{responsecompat.Observe, responsecompat.Enforce} {
			for _, tc := range []struct{ name, status, content, want string }{
				{"mixed", "completed", `[{"type":"output_text","text":"hello"},{"type":"refusal","refusal":"declined"}]`, responsecompat.OutcomeCompleted},
				{"partial", "incomplete", `[{"type":"refusal","refusal":"declined"},{"type":"output_text","text":"hello"}]`, responsecompat.OutcomeIncomplete},
				{"refused", "completed", `[{"type":"refusal","refusal":"declined"}]`, responsecompat.OutcomeRefused},
			} {
				t.Run(fmt.Sprintf("%s/%s/pass=%t", tc.name, mode, pass), func(t *testing.T) {
					up := func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						details := ""
						if tc.status == "incomplete" {
							details = `"incomplete_details":{"reason":"max_output_tokens"},`
						}
						_, _ = fmt.Fprintf(w, `{"id":"resp_%s_%s_%t","object":"response","model":"gpt-6-luna","status":%q,%s"output":[{"type":"message","id":"msg_1","role":"assistant","content":%s}],"usage":{"input_tokens":1,"output_tokens":1}}`, tc.name, mode, pass, tc.status, details, tc.content)
					}
					f := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
						passthrough: pass, rewrite: true, policy: responsesCompatibilityPolicy(mode),
					}, up, responsesCompatibilityBadGateway(t))
					reply := f.post(t, `{"model":"gpt-5.4","input":"hello","stream":false}`)
					f.finish(t)
					require.Equal(t, http.StatusOK, reply.status, string(reply.body))
					require.Len(t, f.a.snapshot(), 1)
					require.Empty(t, f.b.snapshot())
					var outcome string
					require.NoError(t, db.QueryRowContext(t.Context(), "SELECT outcome FROM gateway_compatibility_cohorts WHERE phase=1 AND request_id=$1", reply.header.Get("X-TokenRouter-Request-ID")).Scan(&outcome))
					require.Equal(t, tc.want, outcome)
				})
			}
		}
	}
}

// TestResponsesCohortSurvivesChatFallback checks that a converted A rejected
// before execution can recover through a native Chat candidate. The final
// Responses document belongs to A's original rollout arm and denominator.
func TestResponsesCohortSurvivesChatFallback(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, pass := range []bool{false, true} {
		t.Run(fmt.Sprintf("pass=%t", pass), func(t *testing.T) {
			chat := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"chatcmpl_fallback_%t","object":"chat.completion","model":"gpt-5.4-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello from B"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`, pass)
			}
			f := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
				passthrough: pass, rewrite: true, aOnlyPass: true, bChat: true, switchDirectly: true, policy: responsesCompatibilityPolicy(responsecompat.Enforce),
			}, responsesCompatibilityPreExecutionRefusal(t), chat)
			reply := f.post(t, `{"model":"gpt-5.4","input":"hello","stream":false}`)
			f.finish(t)
			require.Equal(t, http.StatusOK, reply.status, string(reply.body))
			require.Contains(t, string(reply.body), "hello from B")
			require.Len(t, f.a.snapshot(), 1)
			atB := f.b.snapshot()
			require.Len(t, atB, 1)
			require.Equal(t, "/v1/chat/completions", atB[0].path)
			var outcome, arm string
			var originalProvider int
			require.NoError(t, db.QueryRowContext(t.Context(), "SELECT outcome,arm,provider_id FROM gateway_compatibility_cohorts WHERE phase=1 AND request_id=$1", reply.header.Get("X-TokenRouter-Request-ID")).Scan(&outcome, &arm, &originalProvider))
			require.Equal(t, responsecompat.OutcomeCompleted, outcome)
			require.Equal(t, "enforce", arm)
			require.Equal(t, responsesCompatibilityA, originalProvider)
		})
	}
}
