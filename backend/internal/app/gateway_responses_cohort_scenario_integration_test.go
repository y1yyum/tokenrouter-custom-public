//go:build integration

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/requestlog"
	requestlogpg "github.com/TokenFlux/TokenRouter/internal/requestlog/postgres"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/scheduler/rediscache"
	"github.com/TokenFlux/TokenRouter/internal/settings"
	"github.com/TokenFlux/TokenRouter/internal/testutil/rediscontainer"
)

// gateway_responses_cohort_scenario_integration_test.go exercises first-rule
// enrollment, final delivery and settlement in both HTTP dispatch paths.
// Upstream receivers and the client use TCP, and the same migrated database
// stores the cohort, request diagnostics and money movement.

const (
	// The retry hint alone permits recovery. Tool progress and a later
	// authoritative terminal can supersede it before the response is delivered.
	cohortRetryHint = `{"type":"error","error":{"code":"server_is_overloaded","message":"Temporarily overloaded, please retry","execution_started":false}}`
	cohortTool      = `{"type":"response.output_item.done","response_id":"resp_settle","output_index":1,"item":{"id":"tool_1","type":"code_interpreter_call","status":"completed"}}`
	cohortRefusal   = `{"type":"response.failed","response":{"id":"resp_settle","status":"failed","error":{"code":"cyber_policy","message":"request refused"}}}`
)

// responsesCohortRow holds the stored first match and its client-visible result.
type responsesCohortRow struct {
	revision      int64               // first matched revision
	finalRevision sql.NullInt64       // null until final reconciliation
	arm           responsecompat.Mode // first rollout arm
	outcome       sql.NullString      // null while in flight
	applied       bool                // conversion handed to transport
	target        bool                // final streaming-only error
}

// responsesCohortDeliveryBarrier flushes the successful body before letting
// the write return. It gives the client a chance to disconnect after delivery.
type responsesCohortDeliveryBarrier struct {
	gin.ResponseWriter                 // actual HTTP response writer
	clientContext      context.Context // lifetime of the client connection
}

// responsesCohortSlotProbe records each successful release of a Redis member.
type responsesCohortSlotProbe struct {
	scheduler.ConcurrencyCache             // actual Redis slot store
	released                   chan string // released provider member IDs
}

// TestGatewayResponsesCompatibilityCohort links both HTTP dispatches to the
// migrated rollout and billing tables. It checks one logical request through
// upstream recovery, duplicate or conflicting terminals and client cancellation.
func TestGatewayResponsesCompatibilityCohort(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	redisClient := rediscontainer.New(t)
	slots := rediscache.NewConcurrencyCache(redisClient, 15, 30)
	for _, route := range []struct {
		name string
		pass bool
	}{{"ordinary", false}, {"rewritable_passthrough", true}} {
		t.Run(route.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, outcome, source string
				frames                []string
				status                int
				cost                  float64
				switchToB             bool
			}{
				{
					"duplicate_incomplete", responsecompat.OutcomeIncomplete, "terminal",
					[]string{cohortRetryHint, cohortTool, settlementIncomplete, settlementIncomplete},
					200, .11, false,
				},
				{
					"conflicting_terminal_only", responsecompat.OutcomeProtocol502, "terminal",
					[]string{cohortRetryHint, cohortTool, settlementIncomplete, settlementCompleted},
					502, 0, false,
				},
				{
					"conflicting_with_independent_usage", responsecompat.OutcomeProtocol502, "independent",
					[]string{cohortRetryHint, cohortTool, settlementIndependent, settlementIncomplete, settlementCompleted},
					502, .11, false,
				},
				{
					"retry_hint_without_execution", responsecompat.OutcomeCompleted, "",
					[]string{cohortRetryHint},
					200, 0, true,
				},
				{
					"policy_refusal", responsecompat.OutcomeRefused, "",
					[]string{cohortRefusal},
					400, 0, false,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					records := requestlog.NewService(requestlogpg.NewStore(db), 7, t.Logf)
					a := responsesSettlementStream(t, tc.frames, false)
					b := responsesCompatibilitySuccess(t, "resp_cohort_b", responsesCompatibilityFallbackModel, "false")
					fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
						passthrough: route.pass, rewrite: route.pass, switchDirectly: true,
						policy: responsesCompatibilityPolicy(responsecompat.Enforce), observeRequest: records.Observe,
						catalog: responsesSettlementCatalog{},
					}, a, b)
					before := readResponsesCompatibilityLedger(t, db)
					reply := fixture.post(t, settlementPayload)
					fixture.finish(t)
					require.NoError(t, records.Flush(t.Context()))
					id := reply.header.Get("X-TokenRouter-Request-ID")
					assertResponsesCompatibilityWireRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, "true", "text/event-stream", responsesCompatibilityPath, route.pass)
					assertResponsesCohortJSON(t, reply, tc.status)
					entry := readResponsesCohort(t, db, id)
					require.Equal(t, responsecompat.Enforce, entry.arm)
					require.Equal(t, entry.revision, entry.finalRevision.Int64)
					require.True(t, entry.applied)
					require.False(t, entry.target)
					require.Equal(t, sql.NullString{String: tc.outcome, Valid: true}, entry.outcome)
					if tc.switchToB {
						assertResponsesCompatibilityRequest(t, fixture.b.snapshot(), responsesCompatibilityB, responsesCompatibilityFallbackModel, "false", route.pass)
						require.Equal(t, "resp_cohort_b", gjson.GetBytes(reply.body, "id").String())
						return
					}
					require.Empty(t, fixture.b.snapshot())
					record := readResponsesSettlementRequest(t, db, id)
					require.Equal(t, tc.status, record.Status)
					require.Equal(t, 1, record.ResponsesSettlement.CompletionCount)
					require.Equal(t, tc.source, record.ResponsesSettlement.Source)
					if tc.status == http.StatusOK {
						require.Equal(t, "incomplete", gjson.GetBytes(reply.body, "status").String())
						require.Equal(t, "max_output_tokens", gjson.GetBytes(reply.body, "incomplete_details.reason").String())
						require.Len(t, gjson.GetBytes(reply.body, "output.#(id==\"tool_1\")#").Array(), 1)
						require.Equal(t, "ok", gjson.GetBytes(reply.body, "output.0.content.0.text").String())
					} else {
						require.True(t, gjson.GetBytes(reply.body, "error").IsObject())
					}
					if tc.outcome == responsecompat.OutcomeRefused {
						require.Equal(t, "refused", record.ResponsesSettlement.Resolution)
						require.Equal(t, "invalid_request_error", gjson.GetBytes(reply.body, "error.type").String())
						require.Equal(t, "request refused", gjson.GetBytes(reply.body, "error.message").String())
					}
					if tc.cost == 0 {
						require.Equal(t, "pending_review", record.ResponsesSettlement.Disposition)
					}
					assertResponsesSettlementAmount(t, db, fixture, id, before, tc.cost, 0, true)
					t.Logf("D=1 outcome=%s applied=true wallet_delta=%.2f", entry.outcome.String, tc.cost)
				})
			}
			t.Run("in_flight_then_client_gone", func(t *testing.T) {
				assertResponsesCohortDrain(t, db, route.pass, slots)
			})
			t.Run("client_gone_after_delivery", func(t *testing.T) {
				assertResponsesCohortLateDisconnect(t, db, route.pass)
			})
			t.Run("observe_and_off", func(t *testing.T) {
				assertResponsesCohortControls(t, db, route.pass)
			})
			t.Run("shared_window", func(t *testing.T) {
				assertResponsesCohortWindow(t, db, route.pass)
			})
		})
	}
}

// assertResponsesCohortWindow queries both arms of the same policy revision
// while one response is blocked, then reconciles the closed window after EOF.
func assertResponsesCohortWindow(t *testing.T, db *sql.DB, pass bool) {
	t.Helper()
	service := responsecompat.New(settings.New(newResponsesRefreshRepository(db)))
	policy := responsesCompatibilityPolicy(responsecompat.Enforce)
	policy.Rules[0].EnforcePercent = 50
	convertedReceived := make(chan struct{})
	allowTerminal := make(chan struct{})
	release := sync.OnceFunc(func() { close(allowTerminal) })
	success := responsesCompatibilityCompletedStream(t, "resp_cohort_window", responsesCompatibilityLuna)
	fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
		passthrough: pass, rewrite: pass, policy: policy, compatibility: service,
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			// Pause the execution arm after transport handoff so both arms
			// inhabit one database window with distinct delivery states.
			close(convertedReceived)
			select {
			case <-allowTerminal:
				success(w, r)
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Only supports streaming generation requests"}}`)
	}, responsesCompatibilityBadGateway(t), func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), telemetry.ClientRequestID, c.GetHeader("X-Client-Request-ID"))
		c.Request = c.Request.WithContext(ctx)
	})
	t.Cleanup(release)
	status, err := service.Get(t.Context())
	require.NoError(t, err)
	ids := make(map[responsecompat.Mode]string)
	for i := 0; i < 100 && len(ids) < 2; i++ {
		id := fmt.Sprintf("cohort-window-%t-%d", pass, i)
		ids[service.Match(policy.Rules[0].Tuple, id).Mode] = id
	}
	require.NotEmpty(t, ids[responsecompat.Observe])
	require.NotEmpty(t, ids[responsecompat.Enforce])
	windowStart := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), responsesCompatibilityTimeout)
	defer cancel()
	newRequest := func(mode responsecompat.Mode) *http.Request {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gateway.URL+responsesCompatibilityPath, strings.NewReader(settlementPayload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Client-Request-ID", ids[mode])
		return request
	}
	observed, err := fixture.gateway.Client().Do(newRequest(responsecompat.Observe))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, observed.StatusCode)
	require.NoError(t, observed.Body.Close())
	execution := newRequest(responsecompat.Enforce)
	done := make(chan error, 1)
	// Keep HTTP execution alive until the test has sampled its unfinished row.
	go func() {
		response, err := fixture.gateway.Client().Do(execution)
		if err == nil {
			var result json.RawMessage
			err = json.NewDecoder(response.Body).Decode(&result)
			_ = response.Body.Close()
			if err == nil && (response.StatusCode != http.StatusOK || gjson.GetBytes(result, "id").String() != "resp_cohort_window") {
				err = fmt.Errorf("unexpected execution reply status=%d body=%s", response.StatusCode, result)
			}
		}
		done <- err
	}()
	select {
	case <-convertedReceived:
	case <-ctx.Done():
		t.Fatal("execution arm did not reach the upstream")
	}
	windowEnd := time.Now()
	revision := status.Settings.Revision
	require.Eventually(t, func() bool {
		var finished bool
		err := db.QueryRowContext(ctx, "SELECT outcome IS NOT NULL FROM gateway_compatibility_cohorts WHERE phase=1 AND request_id=$1", ids[responsecompat.Observe]).Scan(&finished)
		return err == nil && finished
	}, time.Second, 10*time.Millisecond)
	assertResponsesWindowCounts(t, db, revision, windowStart, windowEnd, false)
	release()
	require.NoError(t, <-done)
	fixture.finish(t)
	assertResponsesWindowCounts(t, db, revision, windowStart, windowEnd, true)
	require.Len(t, fixture.a.snapshot(), 2)
	require.Empty(t, fixture.b.snapshot())
	t.Logf("revision=%d both arms D=1; observe target_400=1; enforce in_flight=1 → completed=1", revision)
}

// assertResponsesWindowCounts verifies the membership and metrics in one
// snapshot. Both reads use the original enrollment bounds and rule revision.
func assertResponsesWindowCounts(t *testing.T, db *sql.DB, revision int64, start, end time.Time, closed bool) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
		SELECT arm, count(*), count(outcome), count(*) FILTER (WHERE outcome IS NULL),
		       count(*) FILTER (WHERE applied), count(*) FILTER (WHERE target_parameter_error),
		       count(*) FILTER (WHERE outcome='completed')
		FROM gateway_compatibility_cohorts
		WHERE phase=1 AND revision=$1 AND enrolled_at >= $2 AND enrolled_at < $3
		      AND group_id=$4 AND provider_id=$5 AND model=$6 AND endpoint=$7 AND NOT client_stream
		GROUP BY arm ORDER BY arm`,
		revision, start, end, responsesCompatibilityGroup, responsesCompatibilityA, responsesCompatibilityLuna, responsesCompatibilityPath)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	want := map[responsecompat.Mode][]int{
		responsecompat.Observe: {1, 1, 0, 0, 1, 0},
		responsecompat.Enforce: {1, 0, 1, 0, 0, 0},
	}
	if closed {
		want[responsecompat.Enforce] = []int{1, 1, 0, 1, 0, 1}
	}
	for rows.Next() {
		var arm responsecompat.Mode
		var d, finished, pending, applied, target, completed int
		require.NoError(t, rows.Scan(&arm, &d, &finished, &pending, &applied, &target, &completed))
		require.Equal(t, d, finished+pending)
		require.Equal(t, want[arm], []int{d, finished, pending, applied, target, completed}, "%s", arm)
		delete(want, arm)
	}
	require.NoError(t, rows.Err())
	require.Empty(t, want)
}

// assertResponsesCohortLateDisconnect cancels a real client after it decodes a
// complete JSON result and checks that the observed completion survives.
func assertResponsesCohortLateDisconnect(t *testing.T, db *sql.DB, pass bool) {
	t.Helper()
	fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
		passthrough: pass, rewrite: pass, policy: responsesCompatibilityPolicy(responsecompat.Observe),
	}, responsesCompatibilitySuccess(t, "resp_cohort_delivered", responsesCompatibilityLuna, "false"),
		responsesCompatibilityBadGateway(t), func(c *gin.Context) {
			c.Writer = responsesCohortDeliveryBarrier{ResponseWriter: c.Writer, clientContext: c.Request.Context()}
		})
	ctx, cancel := context.WithTimeout(t.Context(), responsesCompatibilityTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gateway.URL+responsesCompatibilityPath, strings.NewReader(settlementPayload))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := fixture.gateway.Client().Do(request)
	require.NoError(t, err)
	defer func() { assert.NoError(t, response.Body.Close()) }()
	var result json.RawMessage
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.Equal(t, "resp_cohort_delivered", gjson.GetBytes(result, "id").String())
	require.Equal(t, http.StatusOK, response.StatusCode)
	cancel()
	fixture.finish(t)
	row := readResponsesCohort(t, db, response.Header.Get("X-TokenRouter-Request-ID"))
	require.Equal(t, sql.NullString{String: responsecompat.OutcomeCompleted, Valid: true}, row.outcome)
	require.False(t, row.applied)
}

// Write flushes one accepted body, then waits for the peer to close the
// connection. The writer reports the original byte count and write result.
func (w responsesCohortDeliveryBarrier) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	if err == nil {
		// Expose the complete document on the wire while the handler still
		// owns its outcome, then allow cancellation to race with finalization.
		w.Flush()
		<-w.clientContext.Done()
	}
	return n, err
}

// assertResponsesCohortDrain samples the database before EOF and then cancels
// the active HTTP client. A second request joins the same provider's Redis
// queue while the upstream still holds the first body. Both terminal records
// retain their own outcome and only the first carries billable token evidence.
func assertResponsesCohortDrain(t *testing.T, db *sql.DB, pass bool, slots scheduler.ConcurrencyCache) {
	t.Helper()
	slotProbe := &responsesCohortSlotProbe{ConcurrencyCache: slots, released: make(chan string, 8)}
	upstreamReady := make(chan struct{})
	disconnected := make(chan struct{})
	allowTerminal := make(chan struct{})
	release := sync.OnceFunc(func() { close(allowTerminal) })
	var disconnectOnce sync.Once
	var calls atomic.Int32
	records := requestlog.NewService(requestlogpg.NewStore(db), 7, t.Logf)
	observe := func(record telemetry.RequestRecord) {
		records.Observe(record)
		if record.ClientDisconnectedAt != nil {
			disconnectOnce.Do(func() { close(disconnected) })
		}
	}
	a := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 2 {
			terminal := strings.ReplaceAll(strings.Replace(settlementCompleted, ","+settlementUsage, "", 1), "resp_settle", "resp_cohort_waiter")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", terminal)
			return
		}
		_, _ = fmt.Fprint(w, ": waiting for client\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush preamble: %v", err)
			return
		}
		// Publish a barrier from the live upstream connection so the SQL
		// read occurs only after enrollment and successful transport handoff.
		close(upstreamReady)
		select {
		case <-allowTerminal:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", settlementCompleted)
		case <-r.Context().Done():
			t.Error("upstream closed before final drain")
		}
	}
	localIDs := make(chan string, 1)
	fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
		passthrough: pass, rewrite: pass, catalog: responsesSettlementCatalog{}, concurrency: slotProbe, onlyPrimary: true,
		policy: responsesCompatibilityPolicy(responsecompat.Enforce), observeRequest: observe,
	}, a, responsesCompatibilityBadGateway(t), func(c *gin.Context) {
		id, _ := c.Request.Context().Value(telemetry.ClientRequestID).(string)
		localIDs <- id
	})
	t.Cleanup(release)
	ctx, cancel := context.WithTimeout(t.Context(), responsesCompatibilityTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gateway.URL+responsesCompatibilityPath, strings.NewReader(settlementPayload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	before := readResponsesCompatibilityLedger(t, db)
	done := make(chan error, 1)
	// The client waits for response bytes while the test reads the same
	// in-flight request from SQL. Cancellation then drives the real drain.
	go func() {
		res, err := fixture.gateway.Client().Do(req)
		if res != nil {
			_ = res.Body.Close()
		}
		done <- err
	}()
	select {
	case <-upstreamReady:
	case <-ctx.Done():
		t.Fatal("upstream did not receive the request")
	}
	id := <-localIDs
	inFlight := readResponsesCohort(t, db, id)
	require.False(t, inFlight.outcome.Valid)
	require.Equal(t, responsecompat.Enforce, inFlight.arm)
	require.Equal(t, before, readResponsesCompatibilityLedger(t, db))
	active, err := slots.GetProviderConcurrency(t.Context(), responsesCompatibilityA)
	require.NoError(t, err)
	require.Equal(t, 1, active)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	receivedResponsesRefreshValue(t, disconnected)
	active, err = slots.GetProviderConcurrency(t.Context(), responsesCompatibilityA)
	require.NoError(t, err)
	require.Equal(t, 1, active, "client cancellation cannot return the provider's drain slot")
	// Redis must keep the first physical slot occupied throughout drain. The
	// waiting request has joined the cohort but cannot reach the listener yet.
	waiter := startResponsesRefreshPost(t, fixture, settlementPayload)
	waiterID := receivedResponsesRefreshValue(t, localIDs)
	require.Eventually(t, func() bool {
		waiting, err := slots.GetProviderWaitingCount(t.Context(), responsesCompatibilityA)
		return err == nil && waiting == 1
	}, 2*time.Second, 10*time.Millisecond)
	active, err = slots.GetProviderConcurrency(t.Context(), responsesCompatibilityA)
	require.NoError(t, err)
	require.Equal(t, 1, active)
	require.Len(t, fixture.a.snapshot(), 1)
	require.Empty(t, slotProbe.released)
	require.False(t, readResponsesCohort(t, db, waiterID).outcome.Valid)
	release()
	waiterReply := receivedResponsesRefreshValue(t, waiter)
	require.NoError(t, waiterReply.err)
	assertResponsesCompatibilityReply(t, waiterReply.response, "resp_cohort_waiter", false)
	fixture.finish(t)
	require.NoError(t, records.Flush(t.Context()))
	final := readResponsesCohort(t, db, id)
	require.Equal(t, inFlight.revision, final.revision)
	require.Equal(t, sql.NullString{String: responsecompat.OutcomeClientGone, Valid: true}, final.outcome)
	require.True(t, final.applied)
	require.Equal(t, "completed", readResponsesSettlementRequest(t, db, id).ResponsesSettlement.Resolution)
	require.Len(t, fixture.a.snapshot(), 2)
	require.Empty(t, fixture.b.snapshot())
	waiterResult := readResponsesCohort(t, db, waiterID)
	require.Equal(t, sql.NullString{String: responsecompat.OutcomeCompleted, Valid: true}, waiterResult.outcome)
	require.True(t, waiterResult.applied)
	require.Len(t, slotProbe.released, 2)
	require.NotEqual(t, <-slotProbe.released, <-slotProbe.released)
	active, err = slots.GetProviderConcurrency(t.Context(), responsesCompatibilityA)
	require.NoError(t, err)
	require.Zero(t, active)
	waiting, err := slots.GetProviderWaitingCount(t.Context(), responsesCompatibilityA)
	require.NoError(t, err)
	require.Zero(t, waiting)
	assertResponsesSettlementAmount(t, db, fixture, id, before, .11, 0, true)
	t.Log("first D=1 in_flight=1 → client_gone=1; Redis active=1 wait=1 during drain; two distinct slot releases")
}

// ReleaseProviderSlot delegates removal to Redis and records its successful member ID.
func (p *responsesCohortSlotProbe) ReleaseProviderSlot(ctx context.Context, providerID int64, id string) error {
	err := p.ConcurrencyCache.ReleaseProviderSlot(ctx, providerID, id)
	if err == nil {
		p.released <- id
	}
	return err
}

// assertResponsesCohortControls checks direct synchronous observations and
// confirms that only the target message contributes to the 400 numerator.
func assertResponsesCohortControls(t *testing.T, db *sql.DB, pass bool) {
	t.Helper()
	for _, tc := range []struct {
		name, message, outcome string
		mode                   responsecompat.Mode
		target                 bool
	}{
		{"completed", "", responsecompat.OutcomeCompleted, responsecompat.Observe, false},
		{"streaming_required", "Only supports streaming generation requests", responsecompat.OutcomeParameter400, responsecompat.Observe, true},
		{"unrelated_400", "invalid tool parameter", responsecompat.OutcomeParameter400, responsecompat.Observe, false},
		{"malformed_upstream", "", responsecompat.OutcomeProtocol502, responsecompat.Observe, false},
		{"off", "", "", responsecompat.Off, false},
		{"cold_start", "", "", responsecompat.Off, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := responsesCompatibilitySuccess(t, "resp_cohort_control", responsesCompatibilityLuna, "false")
			status := http.StatusOK
			if tc.message != "" {
				status = http.StatusBadRequest
				a = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(w, `{"error":{"type":"invalid_request_error","message":%q}}`, tc.message)
				}
			}
			if tc.name == "malformed_upstream" {
				status = http.StatusBadGateway
				a = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprint(w, "invalid upstream document")
				}
			}
			options := responsesCompatibilityFixtureOptions{passthrough: pass, rewrite: pass}
			if tc.name != "cold_start" {
				options.policy = responsesCompatibilityPolicy(tc.mode)
			}
			fixture := newResponsesCompatibilityConfiguredFixture(t, db, options, a, responsesCompatibilityBadGateway(t))
			reply := fixture.post(t, settlementPayload)
			fixture.finish(t)
			assertResponsesCohortJSON(t, reply, status)
			assertResponsesCompatibilityRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, "false", pass)
			require.Empty(t, fixture.b.snapshot())
			id := reply.header.Get("X-TokenRouter-Request-ID")
			if tc.mode == responsecompat.Off {
				var count int
				require.NoError(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM gateway_compatibility_cohorts WHERE request_id=$1", id).Scan(&count))
				require.Zero(t, count)
				return
			}
			entry := readResponsesCohort(t, db, id)
			require.Equal(t, responsecompat.Observe, entry.arm)
			require.Equal(t, sql.NullString{String: tc.outcome, Valid: true}, entry.outcome)
			require.False(t, entry.applied)
			require.Equal(t, tc.target, entry.target)
		})
	}
}

// readResponsesCohort checks one enrolled logical request and reconciles its
// final and in-flight counts before reading the original rule identity.
func readResponsesCohort(t *testing.T, db *sql.DB, id string) responsesCohortRow {
	t.Helper()
	require.NotEmpty(t, id)
	var total, finished, pending int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*), count(outcome), count(*) FILTER (WHERE outcome IS NULL)
		FROM gateway_compatibility_cohorts WHERE phase=1 AND request_id=$1`, id).Scan(&total, &finished, &pending))
	require.Equal(t, 1, total)
	require.Equal(t, total, finished+pending)
	var row responsesCohortRow
	var group, provider int64
	var model, endpoint string
	var stream bool
	var ended sql.NullTime
	var duration sql.NullInt64
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT revision,final_revision,arm,outcome,applied,target_parameter_error,group_id,provider_id,model,endpoint,client_stream,finished_at,duration_ms
		FROM gateway_compatibility_cohorts WHERE phase=1 AND request_id=$1`, id).
		Scan(&row.revision, &row.finalRevision, &row.arm, &row.outcome, &row.applied, &row.target, &group, &provider, &model, &endpoint, &stream, &ended, &duration))
	require.Positive(t, row.revision)
	require.EqualValues(t, responsesCompatibilityGroup, group)
	require.EqualValues(t, responsesCompatibilityA, provider)
	require.Equal(t, responsesCompatibilityLuna, model)
	require.Equal(t, responsesCompatibilityPath, endpoint)
	require.False(t, stream)
	require.Equal(t, row.outcome.Valid, ended.Valid)
	require.Equal(t, row.outcome.Valid, duration.Valid)
	require.Equal(t, row.outcome.Valid, row.finalRevision.Valid)
	return row
}

// assertResponsesCohortJSON rejects concatenated JSON documents and SSE bytes
// on the synchronous client connection before semantic outcome assertions.
func assertResponsesCohortJSON(t *testing.T, reply responsesCompatibilityReply, status int) {
	t.Helper()
	require.Equal(t, status, reply.status, "%s", reply.body)
	require.Contains(t, reply.header.Get("Content-Type"), "application/json")
	require.True(t, gjson.ValidBytes(reply.body), "%s", reply.body)
	require.NotContains(t, string(reply.body), "data: ")
}
