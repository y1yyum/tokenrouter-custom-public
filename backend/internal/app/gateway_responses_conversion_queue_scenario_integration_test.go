//go:build integration

package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	gatewayhttptest "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi/testkit"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/pkg/requestcontext"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// gateway_responses_conversion_queue_scenario_integration_test.go saturates
// the shared aggregation budget with actual inbound and outbound HTTP traffic.
// The fifth request targets a different rule and provider. Its first cohort
// row and returned prefetch expose the point where it is waiting without
// taking distributed provider capacity or reaching the upstream listener.

const (
	// responsesQueuePrevious binds the fifth request and controls to provider B.
	responsesQueuePrevious = "resp_queue_previous"
)

// responsesQueueTraffic counts simultaneous converted HTTP messages across
// both test receivers, excluding the native and observation control requests.
type responsesQueueTraffic struct {
	active atomic.Int32 // responses still being sent upstream
	peak   atomic.Int32 // highest concurrent converted response count
}

// TestGatewayResponsesConversionQueue runs the production HTTP paths against
// two separate listeners and the migrated cohort table. For each dispatch
// path, four live conversions occupy the budget while a fifth either waits
// for body completion, reverts to synchronous, or stops without sending.
func TestGatewayResponsesConversionQueue(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	for _, route := range []struct {
		name string
		pass bool
	}{{"ordinary", false}, {"rewritable_passthrough", true}} {
		t.Run(route.name, func(t *testing.T) {
			for _, scenario := range []string{"release_with_large_responses", "rule_closed", "global_closed", "snapshot_expired", "client_canceled", "server_abort", "total_deadline"} {
				t.Run(scenario, func(t *testing.T) { runResponsesConversionQueue(t, db, route.pass, scenario) })
			}
		})
	}
}

// runResponsesConversionQueue observes the fifth request through enrollment,
// yielded prefetch and final outcome. The upstream tail remains gated while
// checking cancellation, expiration, rule closure and the bypass controls.
func runResponsesConversionQueue(t *testing.T, db *sql.DB, pass bool, scenario string) {
	t.Helper()
	const syncID, streamID = "resp_queue_sync", "resp_queue_sse"
	service := responsecompat.New(settings.New(newResponsesRefreshRepository(db)))
	policy := responsesCompatibilityPolicy(responsecompat.Enforce)
	other := policy.Rules[0]
	other.ProviderID, other.UpstreamModel, other.EnforcePercent = responsesCompatibilityB, responsesCompatibilityFallbackModel, 50
	policy.Rules = append(policy.Rules, other)
	// Choose B's IDs from the real bucket function after publication. Since
	// only B samples at 50%, A's four controlled requests always convert.
	first := saveResponsesRefreshPolicy(t, service, *policy).Settings.Revision
	waitID := responsesQueueArmID(t, service, other.Tuple, responsecompat.Enforce, fmt.Sprintf("queue-%t-%s-fifth", pass, scenario))
	observeID := responsesQueueArmID(t, service, other.Tuple, responsecompat.Observe, waitID+"-observe")
	var acquired atomic.Int32
	var abort context.CancelFunc
	abortReady, ingressReady := make(chan struct{}), make(chan struct{})
	allowIngress := make(chan struct{})
	openIngress := sync.OnceFunc(func() { close(allowIngress) })
	defer openIngress()
	var total time.Duration
	if scenario == "total_deadline" {
		total = 2 * time.Second
	}
	slots := &gatewayhttptest.ConcurrencyHooks{
		AcquireProviderSlotFn: func(ctx context.Context, providerID int64, _ int, _ string) (bool, error) {
			if scenario == "total_deadline" && providerID == responsesCompatibilityB {
				// Give the queued request an earlier entry time without changing
				// its configured budget or the clocks of the four live conversions.
				close(ingressReady)
				select {
				case <-allowIngress:
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			acquired.Add(1)
			return true, nil
		},
		AcquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	traffic := &responsesQueueTraffic{}
	arrived, allowTail := make(chan struct{}, 4), make(chan struct{}, 4)
	var progress string
	if scenario == "release_with_large_responses" {
		// Keep the production refresh lifecycle active while the race build
		// processes multi-megabyte bodies across all four readers.
		startResponsesRefreshPoller(t, service)
		progress = responsesQueueLargeProgress()
	}
	native := responsesCompatibilitySuccess(t, "resp_queue_native", responsesCompatibilityLuna, "true")
	ordinary := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			native(w, r)
			return
		}
		traffic.enter()
		defer traffic.active.Add(-1)
		responsesQueueHoldTail(w, r, arrived, allowTail, progress, streamID)
	}
	streamB := responsesCompatibilityCompletedStream(t, streamID, responsesCompatibilityFallbackModel)
	syncB := responsesCompatibilitySuccess(t, syncID, responsesCompatibilityFallbackModel, "false")
	second := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "text/event-stream" {
			traffic.enter()
			defer traffic.active.Add(-1)
			streamB(w, r)
			return
		}
		syncB(w, r)
	}
	mixedEntries := scenario == "release_with_large_responses"
	fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
		passthrough: pass, rewrite: true, compatibility: service, concurrency: slots,
		aOnlyPass: mixedEntries && pass, bOnlyPass: mixedEntries && !pass,
		previousBID: responsesQueuePrevious, providerLimit: 8, acceptOverride: "text/plain", total: total,
	}, ordinary, second, func(c *gin.Context) {
		id := c.GetHeader("X-Client-Request-ID")
		ctx := context.WithValue(c.Request.Context(), telemetry.ClientRequestID, id)
		if id == waitID && scenario == "server_abort" {
			ctx, abort = requestcontext.WithAbort(ctx)
			defer abort()
			close(abortReady)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	t.Cleanup(func() { close(allowTail) })
	queuedPayload := fmt.Sprintf(`{"model":"gpt-5.4","input":"return ok","max_tokens":20,"previous_response_id":%q}`, responsesQueuePrevious)
	var fifth <-chan refreshReply
	var cancelFifth context.CancelFunc
	if scenario == "total_deadline" {
		fifth, cancelFifth = startResponsesQueuePost(t, fixture, waitID, queuedPayload)
		receivedResponsesRefreshValue(t, ingressReady)
		// Separate the entry deadlines so the fifth can expire with all four slots still held.
		<-time.After(600 * time.Millisecond)
	}
	holders := make([]<-chan refreshReply, 4)
	for i := range holders {
		holders[i], _ = startResponsesQueuePost(t, fixture, fmt.Sprintf("%s-holder-%d", waitID, i), refreshOmitted)
	}
	for range holders {
		receivedResponsesRefreshValue(t, arrived)
	}
	require.EqualValues(t, 4, traffic.active.Load())
	if fifth == nil {
		fifth, cancelFifth = startResponsesQueuePost(t, fixture, waitID, queuedPayload)
	} else {
		openIngress()
	}
	defer cancelFifth()
	require.Eventually(t, func() bool { return atomic.LoadInt32(&slots.ReleaseProviderCalled) == 1 }, time.Second, 5*time.Millisecond)
	require.EqualValues(t, 5, acquired.Load(), "the fifth prefetched a provider slot, then returned it")
	require.Len(t, fixture.a.snapshot(), 4)
	require.Empty(t, fixture.b.snapshot())
	assertResponsesQueueCohort(t, db, waitID, first, sql.NullInt64{}, responsecompat.Enforce, "", false)
	var expectedFinal int64
	var expectedOutcome string
	var applied bool
	switch scenario {
	case "release_with_large_responses":
		observed, _ := startResponsesQueuePost(t, fixture, observeID, queuedPayload)
		nativeReply, _ := startResponsesQueuePost(t, fixture, waitID+"-native", strings.Replace(refreshExplicit, `"stream":false`, `"stream":true`, 1))
		assertResponsesCompatibilityReply(t, responsesQueueReply(t, observed), syncID, false)
		assertResponsesCompatibilityReply(t, responsesQueueReply(t, nativeReply), "resp_queue_native", true)
		assertResponsesQueueCohort(t, db, observeID, first, sql.NullInt64{Int64: first, Valid: true}, responsecompat.Observe, responsecompat.OutcomeCompleted, false)
		require.EqualValues(t, 4, traffic.active.Load(), "both bypass requests finished while the converters were held")
		require.Len(t, fixture.b.snapshot(), 1)
		require.Empty(t, fifth)
		allowTail <- struct{}{}
		assertResponsesCompatibilityReply(t, responsesQueueReply(t, fifth), streamID, false)
		assertResponsesCompatibilityWireRequest(t, fixture.b.snapshot()[1:], responsesCompatibilityB, responsesCompatibilityFallbackModel, "true", "text/event-stream", responsesCompatibilityPath, !pass)
		expectedFinal, expectedOutcome, applied = first, responsecompat.OutcomeCompleted, true
	case "rule_closed", "global_closed", "snapshot_expired":
		switch scenario {
		case "rule_closed":
			policy.Rules[1].Mode = responsecompat.Off
		case "global_closed":
			policy.Mode = responsecompat.Off
		}
		if scenario != "snapshot_expired" {
			expectedFinal = saveResponsesRefreshPolicy(t, service, *policy).Settings.Revision
		}
		assertResponsesCompatibilityReply(t, responsesQueueReply(t, fifth), syncID, false)
		require.EqualValues(t, 4, traffic.active.Load(), "withdrawal woke the fifth before any holder finished")
		assertResponsesCompatibilityWireRequest(t, fixture.b.snapshot(), responsesCompatibilityB, responsesCompatibilityFallbackModel, "", "application/json", responsesCompatibilityPath, pass)
		expectedOutcome = responsecompat.OutcomeCompleted
	case "client_canceled":
		cancelFifth()
		require.ErrorIs(t, receivedResponsesRefreshValue(t, fifth).err, context.Canceled)
		expectedFinal, expectedOutcome = first, responsecompat.OutcomeClientGone
	case "server_abort":
		receivedResponsesRefreshValue(t, abortReady)
		abort()
		require.Equal(t, 499, responsesQueueReply(t, fifth).status)
		expectedFinal, expectedOutcome = first, responsecompat.OutcomeClientGone
	case "total_deadline":
		assertResponsesCohortJSON(t, responsesQueueReply(t, fifth), http.StatusGatewayTimeout)
		require.EqualValues(t, 4, traffic.active.Load(), "the fifth hit its original deadline with all slots occupied")
		expectedFinal, expectedOutcome = first, responsecompat.OutcomeTimeout504
	}
	assertResponsesQueueCohort(t, db, waitID, first, sql.NullInt64{Int64: expectedFinal, Valid: true}, responsecompat.Enforce, expectedOutcome, applied)
	if expectedOutcome != responsecompat.OutcomeCompleted {
		require.Empty(t, fixture.b.snapshot(), "a stopped waiter cannot send or switch providers")
		require.EqualValues(t, 5, acquired.Load(), "a stopped waiter never reacquired provider capacity")
	}
	for range holders {
		allowTail <- struct{}{}
	}
	for _, reply := range holders {
		response := responsesQueueReply(t, reply)
		require.Equal(t, http.StatusOK, response.status)
		require.Equal(t, streamID, gjson.GetBytes(response.body, "id").String())
		if progress != "" {
			require.Len(t, gjson.GetBytes(response.body, "output.0.content.0.text").String(), 8<<20)
		}
	}
	fixture.finish(t)
	require.EqualValues(t, 4, traffic.peak.Load())
	require.Equal(t, acquired.Load(), atomic.LoadInt32(&slots.ReleaseProviderCalled))
	t.Logf("peak converted HTTP=%d progress_bytes_per_holder=%d provider_acquired=%d provider_released=%d first_revision=%d final_revision=%d outcome=%s", traffic.peak.Load(), len(progress), acquired.Load(), atomic.LoadInt32(&slots.ReleaseProviderCalled), first, expectedFinal, expectedOutcome)
}

// responsesQueueArmID finds a fixture ID assigned to the requested arm under the published rule.
func responsesQueueArmID(t *testing.T, service *responsecompat.Service, tuple responsecompat.Tuple, arm responsecompat.Mode, prefix string) string {
	t.Helper()
	for i := range 100 {
		id := fmt.Sprintf("%s-%d", prefix, i)
		if service.Match(tuple, id).Mode == arm {
			return id
		}
	}
	t.Fatalf("no fixture ID assigned to %s", arm)
	return ""
}

// enter updates the high-water mark after this converted request actually reaches TCP.
func (traffic *responsesQueueTraffic) enter() {
	active := traffic.active.Add(1)
	for peak := traffic.peak.Load(); active > peak; peak = traffic.peak.Load() {
		if traffic.peak.CompareAndSwap(peak, active) {
			return
		}
	}
}

// responsesQueueHoldTail sends application data and holds the terminal frame.
// Releasing one tail allows the corresponding gateway reader to reach EOF
// before its occupied aggregation slot can serve another conversion.
func responsesQueueHoldTail(w http.ResponseWriter, r *http.Request, arrived chan<- struct{}, allowTail <-chan struct{}, progress, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	if _, err := fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"status\":\"in_progress\"}}\n\n%s", id, progress); err != nil {
		return
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	arrived <- struct{}{}
	select {
	case <-allowTail:
	case <-r.Context().Done():
		return
	}
	if progress == "" {
		progress = fmt.Sprintf("data: {\"type\":\"response.output_text.delta\",\"response_id\":%q,\"item_id\":\"msg_queue\",\"output_index\":0,\"content_index\":0,\"delta\":\"ok\"}\n\n", id)
		_, _ = io.WriteString(w, progress)
	}
	_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n", id)
}

// responsesQueueLargeProgress builds eight MiB of output from 64 modest SSE frames.
func responsesQueueLargeProgress() string {
	chunk := strings.Repeat("x", 128<<10)
	var body strings.Builder
	for range 64 {
		fmt.Fprintf(&body, "data: {\"type\":\"response.output_text.delta\",\"response_id\":\"resp_queue_sse\",\"item_id\":\"msg_queue\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n", chunk)
	}
	return body.String()
}

// startResponsesQueuePost starts a cancellable TCP request under a fixed cohort ID.
func startResponsesQueuePost(t *testing.T, fixture *responsesCompatibilityFixture, id, payload string) (<-chan refreshReply, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gateway.URL+responsesCompatibilityPath, strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Request-ID", id)
	result := make(chan refreshReply, 1)
	go func() {
		defer cancel()
		response, err := fixture.gateway.Client().Do(req)
		if err != nil {
			result <- refreshReply{err: err}
			return
		}
		body, err := io.ReadAll(response.Body)
		err = errors.Join(err, response.Body.Close())
		result <- refreshReply{response: responsesCompatibilityReply{status: response.StatusCode, header: response.Header.Clone(), body: body}, err: err}
	}()
	return result, cancel
}

// responsesQueueReply returns the complete downstream HTTP body for a live client.
func responsesQueueReply(t *testing.T, reply <-chan refreshReply) responsesCompatibilityReply {
	t.Helper()
	result := receivedResponsesRefreshValue(t, reply)
	require.NoError(t, result.err)
	return result.response
}

// assertResponsesQueueCohort checks that the initial rule and arm survive
// queueing and that finalization records only the last evaluated revision.
func assertResponsesQueueCohort(t *testing.T, db *sql.DB, id string, first int64, final sql.NullInt64, arm responsecompat.Mode, outcome string, applied bool) {
	t.Helper()
	var got responsesCohortRow
	var target int64
	require.Eventually(t, func() bool {
		err := db.QueryRowContext(t.Context(), `SELECT revision, final_revision, arm, outcome, applied, provider_id
			FROM gateway_compatibility_cohorts WHERE phase=$1 AND request_id=$2`, responsecompat.ResponsesPhase, id).
			Scan(&got.revision, &got.finalRevision, &got.arm, &got.outcome, &got.applied, &target)
		return err == nil && got.outcome.Valid == (outcome != "")
	}, time.Second, 5*time.Millisecond)
	require.EqualValues(t, responsesCompatibilityB, target)
	require.Equal(t, first, got.revision)
	require.Equal(t, final, got.finalRevision)
	require.Equal(t, arm, got.arm)
	require.Equal(t, outcome, got.outcome.String)
	require.Equal(t, applied, got.applied)
}
