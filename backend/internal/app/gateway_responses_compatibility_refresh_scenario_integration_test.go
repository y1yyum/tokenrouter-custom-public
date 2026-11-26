//go:build integration

package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/ent"
	gatewayhttptest "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi/testkit"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/httpclient"
	"github.com/TokenFlux/TokenRouter/internal/infra/httpclient/tlsfingerprint"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/settings"
	settingspg "github.com/TokenFlux/TokenRouter/internal/settings/postgres"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// gateway_responses_compatibility_refresh_scenario_integration_test.go connects
// the Responses execution and compatibility snapshot to a shared PostgreSQL
// primary and real TCP receivers. The queue gate and read delays expose periods
// when admission and dispatch can observe different revisions.

const (
	// These requests exercise both legal synchronous representations on the wire.
	refreshExplicit = `{"model":"gpt-5.4","input":"return ok","max_tokens":20,"stream":false}`
	refreshOmitted  = `{"model":"gpt-5.4","input":"return ok","max_tokens":20}`
)

// responsesCompatibilityTransportTap observes the gateway's final request before real HTTP transport.
type responsesCompatibilityTransportTap struct {
	httpclient.UpstreamTransport                     // real HTTP client
	inspect                      func(*http.Request) // synchronous read-only observer
}

// responsesRefreshRepository adds reproducible slow and delayed reads to PostgreSQL.
type responsesRefreshRepository struct {
	settings.Repository                  // real Ent settings repository
	db                  *sql.DB          // PostgreSQL primary connection
	slow                atomic.Bool      // delay reads in SQL
	fail                atomic.Bool      // select from an unavailable PostgreSQL relation
	firstSlow           chan refreshRead // completed SQL timeout observations
	captured            chan time.Time   // time that the old revision was read
	release             chan struct{}    // permission to return captured result
}

// refreshDelayReadKey selects the manually launched read for delayed return.
type refreshDelayReadKey struct{}

// refreshRead records timing and cancellation of a slow authoritative SQL statement.
type refreshRead struct {
	started time.Time     // monotonic query start
	elapsed time.Duration // SQL round trip
	err     error         // context result
}

// responsesRefreshSlots exposes the point where the HTTP helper joins the provider queue.
type responsesRefreshSlots struct {
	gatewayhttptest.ConcurrencyHooks               // ordinary scheduler cache behavior
	queued                           chan struct{} // one owned wait registration
}

// refreshReply carries a complete client response or its transport error.
type refreshReply struct {
	response responsesCompatibilityReply // HTTP status, headers and bytes
	err      error                       // request or body-read error
}

// TestGatewayResponsesCompatibilityRefresh tests two gateway handlers and
// independent pollers against one migrated primary. Both entry points cover
// withdrawal, sticky cohort assignment, repaired sends and work in flight.
func TestGatewayResponsesCompatibilityRefresh(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	t.Run("independent_instances", func(t *testing.T) {
		testResponsesRefreshInstances(t, db)
	})
	for _, route := range []struct {
		name string
		pass bool
	}{{"ordinary", false}, {"rewritable_passthrough", true}} {
		t.Run(route.name, func(t *testing.T) {
			for _, scenario := range []string{"revocation", "observation", "expiry"} {
				t.Run("queued_"+scenario, func(t *testing.T) {
					testResponsesRefreshQueue(t, db, route.pass, scenario)
				})
			}
			t.Run("field_repair", func(t *testing.T) {
				testResponsesRefreshFieldRepair(t, db, route.pass)
			})
			t.Run("candidate_switch", func(t *testing.T) {
				testResponsesRefreshSwitch(t, db, route.pass)
			})
			t.Run("in_flight", func(t *testing.T) {
				testResponsesRefreshInFlight(t, db, route.pass)
			})
		})
	}
}

// testResponsesRefreshInstances measures shutdown from the writer's successful
// database commit observation. The second gateway has its own store and poller,
// and receives neither the writer's local publication nor any notification.
func testResponsesRefreshInstances(t *testing.T, db *sql.DB) {
	writer := responsecompat.New(settings.New(newResponsesRefreshRepository(db)))
	repo := &responsesRefreshRepository{
		Repository: newResponsesRefreshRepository(db), db: db,
		firstSlow: make(chan refreshRead, 8), captured: make(chan time.Time, 1), release: make(chan struct{}),
	}
	reader := responsecompat.New(settings.New(repo))
	testPolicy := responsesCompatibilityPolicy(responsecompat.Enforce)
	tuple := testPolicy.Rules[0].Tuple
	settingsForInstances := []*responsecompat.Service{writer, reader}
	fixtures := make([]*responsesCompatibilityFixture, 0, len(settingsForInstances))
	for i, instance := range settingsForInstances {
		id := fmt.Sprintf("resp_refresh_node%d", i)
		jsonSuccess := responsesCompatibilitySuccess(t, id, responsesCompatibilityLuna, "false")
		streamSuccess := responsesCompatibilityCompletedStream(t, id, responsesCompatibilityLuna)
		upstream := func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept") == "text/event-stream" {
				streamSuccess(w, r)
				return
			}
			jsonSuccess(w, r)
		}
		fixtures = append(fixtures, newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
			passthrough: i == 1, rewrite: true, compatibility: instance,
		}, upstream, responsesCompatibilityBadGateway(t)))
	}
	startResponsesRefreshPoller(t, writer)
	startResponsesRefreshPoller(t, reader)
	for _, fault := range []string{"rule_off_no_notification", "global_off_late_read", "global_off_slow_sql", "rule_off_unreadable_sql"} {
		t.Run(fault, func(t *testing.T) {
			repo.slow.Store(false)
			repo.fail.Store(false)
			on := saveResponsesRefreshPolicy(t, writer, *responsesCompatibilityPolicy(responsecompat.Enforce))
			require.Eventually(t, func() bool {
				match := reader.Match(tuple, "propagation")
				return match.Revision == on.Settings.Revision && match.Mode == responsecompat.Enforce
			}, 2*time.Second, 10*time.Millisecond)
			offsets := make([]int, len(fixtures))
			for i, fixture := range fixtures {
				offsets[i] = len(fixture.a.snapshot())
				reply := fixture.post(t, refreshExplicit)
				require.Equal(t, http.StatusOK, reply.status, string(reply.body))
				assertResponsesCompatibilityWireRequest(t, fixture.a.snapshot()[offsets[i]:], responsesCompatibilityA, responsesCompatibilityLuna, "true", "text/event-stream", responsesCompatibilityPath, i == 1)
				offsets[i]++
			}

			var releaseLate func()
			var lateRead <-chan error
			var lateStarted time.Time
			switch fault {
			case "global_off_late_read":
				// The held result comes from the real primary before the write. Allow
				// a later poll to apply off before returning this expired result.
				releaseLate = sync.OnceFunc(func() { close(repo.release) })
				t.Cleanup(releaseLate)
				done := make(chan error, 1)
				ctx := context.WithValue(t.Context(), refreshDelayReadKey{}, true)
				go func() { done <- reader.Refresh(ctx) }()
				lateRead = done
				lateStarted = receivedResponsesRefreshValue(t, repo.captured)
			case "global_off_slow_sql":
				repo.slow.Store(true)
			case "rule_off_unreadable_sql":
				repo.fail.Store(true)
			}
			replacement := on.Settings
			if strings.HasPrefix(fault, "global") {
				replacement.Mode = responsecompat.Off
			} else {
				replacement.Rules[0].Mode = responsecompat.Off
			}
			beforeWrite := time.Now()
			saved := saveResponsesRefreshPolicy(t, writer, replacement)
			commitObserved := time.Now()
			require.Equal(t, saved.Settings.Revision, writer.Match(tuple, "propagation").Revision)
			for i, instance := range settingsForInstances {
				require.Eventually(t, func() bool {
					return instance.Match(tuple, "propagation").Mode == responsecompat.Off
				}, 5*time.Second, 5*time.Millisecond)
				t.Logf("instance=%d commit-return-to-off=%s save-start-to-off=%s", i+1, time.Since(commitObserved), time.Since(beforeWrite))
				require.LessOrEqual(t, time.Since(beforeWrite), 5*time.Second)
				reply := fixtures[i].post(t, refreshExplicit)
				require.Equal(t, http.StatusOK, reply.status, string(reply.body))
				assertResponsesCompatibilityWireRequest(t, fixtures[i].a.snapshot()[offsets[i]:], responsesCompatibilityA, responsesCompatibilityLuna, "false", "application/json", responsesCompatibilityPath, i == 1)
				require.Empty(t, fixtures[i].b.snapshot())
			}
			if releaseLate != nil {
				require.Equal(t, saved.Settings.Revision, reader.Match(tuple, "propagation").Revision)
				// Hold the captured value past its authorization deadline after the
				// independent ticker has published the newer off revision.
				timer := time.NewTimer(time.Until(lateStarted.Add(1100 * time.Millisecond)))
				defer timer.Stop()
				receivedResponsesRefreshValue(t, timer.C)
				releaseLate()
				require.ErrorIs(t, receivedResponsesRefreshValue(t, lateRead), context.DeadlineExceeded)
				require.Equal(t, responsecompat.Decision{Mode: responsecompat.Off, Revision: saved.Settings.Revision}, reader.Match(tuple, "propagation"))
				t.Logf("late primary value released after %s and discarded", time.Since(lateStarted))
			}
			if fault == "global_off_slow_sql" {
				slow := receivedResponsesRefreshValue(t, repo.firstSlow)
				require.ErrorIs(t, slow.err, context.DeadlineExceeded)
				require.InDelta(t, float64(time.Second), float64(slow.elapsed), float64(250*time.Millisecond))
				next := receivedResponsesRefreshValue(t, repo.firstSlow)
				require.ErrorIs(t, next.err, context.DeadlineExceeded)
				interval := next.started.Sub(slow.started)
				require.InDelta(t, float64(time.Second), float64(interval), float64(250*time.Millisecond))
				t.Logf("primary SQL pg_sleep(2) interrupted after %s and %s, start spacing %s", slow.elapsed, next.elapsed, interval)
			}
		})
	}
	for _, fixture := range fixtures {
		fixture.finish(t)
	}
}

// testResponsesRefreshQueue changes policy after an actual provider queue
// registration. It checks the pre-send cohort, the transport's reopenable body,
// the real wire image, and the reader choice after the wait releases.
func testResponsesRefreshQueue(t *testing.T, db *sql.DB, pass bool, scenario string) {
	service := responsecompat.New(settings.New(newResponsesRefreshRepository(db)))
	policy := responsesCompatibilityPolicy(responsecompat.Enforce)
	requestID := fmt.Sprintf("hot-refresh-%t-%s", pass, scenario)
	if scenario == "observation" {
		// Select a fixed fixture ID which crosses the configured rollout interval.
		for bucket := 0; ; bucket++ {
			candidate := fmt.Sprintf("%s-%d", requestID, bucket)
			hash := fnv.New64a()
			_, _ = hash.Write([]byte(candidate))
			if value := hash.Sum64() % 100; value >= 10 && value < 50 {
				requestID = candidate
				break
			}
		}
		policy.Rules[0].EnforcePercent = 10
	}
	first := saveResponsesRefreshPolicy(t, service, *policy)
	var open atomic.Bool
	slots := &responsesRefreshSlots{queued: make(chan struct{}, 1)}
	slots.AcquireUserSlotFn = func(context.Context, int64, int, string) (bool, error) { return true, nil }
	slots.AcquireProviderSlotFn = func(context.Context, int64, int, string) (bool, error) { return open.Load(), nil }
	reopened := make(chan []byte, 1)
	var dispatches atomic.Int32
	const responseID = "resp_refresh_queue"
	fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
		passthrough: pass, rewrite: true, compatibility: service, concurrency: slots, acceptOverride: "text/plain",
		transport: openai.ResponsesTransportOptions{FirstOutputTimeout: time.Nanosecond, OnDispatch: func() { dispatches.Add(1) }},
		tapTransport: func(r *http.Request) {
			if r.GetBody == nil {
				t.Error("synchronous transport cannot reopen the body")
				return
			}
			body, err := r.GetBody()
			if err != nil {
				t.Errorf("reopen synchronous request: %v", err)
				return
			}
			value, err := io.ReadAll(body)
			if err != nil {
				t.Errorf("read reopened request: %v", err)
			}
			if err := body.Close(); err != nil {
				t.Errorf("close reopened request: %v", err)
			}
			if int64(len(value)) != r.ContentLength {
				t.Errorf("reopened length %d differs from HTTP length %d", len(value), r.ContentLength)
			}
			reopened <- value
		},
	}, responsesCompatibilitySuccess(t, responseID, responsesCompatibilityLuna, "false"), responsesCompatibilityBadGateway(t), func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), telemetry.ClientRequestID, requestID)
		c.Request = c.Request.WithContext(ctx)
	})
	payload, stream := refreshExplicit, "false"
	if pass {
		payload, stream = refreshOmitted, ""
	}
	pending := startResponsesRefreshPost(t, fixture, payload)
	receivedResponsesRefreshValue(t, slots.queued)
	require.Empty(t, fixture.a.snapshot())
	require.Empty(t, reopened)
	arm := responsecompat.Enforce
	accept := "application/json"
	updated := first.Settings
	if scenario == "observation" {
		arm, accept = responsecompat.Observe, "text/plain"
		updated.Rules[0].EnforcePercent = 50
	} else {
		updated.Mode = responsecompat.Off
	}
	assertResponsesRefreshCohort(t, db, requestID, first.Settings.Revision, arm, sql.NullInt64{}, false)
	var finalRevision int64
	if scenario == "expiry" {
		// No poller is running for this fixture. Let the already enrolled
		// request lose its snapshot while the provider queue holds the send.
		require.Eventually(t, func() bool {
			return service.Match(policy.Rules[0].Tuple, requestID) == (responsecompat.Decision{Mode: responsecompat.Off})
		}, 4*time.Second, 10*time.Millisecond)
	} else {
		finalRevision = saveResponsesRefreshPolicy(t, service, updated).Settings.Revision
		if scenario == "observation" {
			require.Equal(t, responsecompat.Enforce, service.Match(policy.Rules[0].Tuple, requestID).Mode)
		}
	}
	unblocked := time.Now()
	open.Store(true)
	received := receivedResponsesRefreshValue(t, pending)
	require.NoError(t, received.err)
	assertResponsesCompatibilityReply(t, received.response, responseID, false)
	fixture.finish(t)
	t.Logf("queue release-to-client-EOF=%s", time.Since(unblocked))
	requests := fixture.a.snapshot()
	assertResponsesCompatibilityWireRequest(t, requests, responsesCompatibilityA, responsesCompatibilityLuna, stream, accept, responsesCompatibilityPath, pass)
	require.Equal(t, requests[0].body, receivedResponsesRefreshValue(t, reopened))
	require.Empty(t, fixture.b.snapshot())
	require.Zero(t, dispatches.Load())
	require.EqualValues(t, 1, atomic.LoadInt32(&slots.ReleaseProviderCalled))
	assertResponsesRefreshCohort(t, db, requestID, first.Settings.Revision, arm, sql.NullInt64{Int64: finalRevision, Valid: true}, false)
	t.Logf("queue %s: enrollment=%d final_evaluation=%d arm=%s applied=false", scenario, first.Settings.Revision, finalRevision, arm)
}

// testResponsesRefreshFieldRepair closes the rule after the first converted
// request reaches the server. The permitted local resend uses the repaired
// synchronous body with its original mapped model and a JSON-only Accept.
func testResponsesRefreshFieldRepair(t *testing.T, db *sql.DB, pass bool) {
	service := responsecompat.New(settings.New(newResponsesRefreshRepository(db)))
	on := saveResponsesRefreshPolicy(t, service, *responsesCompatibilityPolicy(responsecompat.Enforce))
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var count atomic.Int32
	success := responsesCompatibilitySuccess(t, "resp_refresh_repair", responsesCompatibilityLuna, "false")
	a := func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) > 1 {
			success(w, r)
			return
		}
		received <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"unknown_parameter","param":"truncation","message":"Unknown parameter: truncation","execution_started":false}}`)
	}
	fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
		passthrough: pass, rewrite: true, compatibility: service, acceptOverride: "text/plain",
	}, a, responsesCompatibilityBadGateway(t))
	t.Cleanup(unblock)
	pending := startResponsesRefreshPost(t, fixture, replayRequest)
	receivedResponsesRefreshValue(t, received)
	on.Settings.Rules[0].Mode = responsecompat.Off
	saveResponsesRefreshPolicy(t, service, on.Settings)
	unblock()
	reply := receivedResponsesRefreshValue(t, pending)
	require.NoError(t, reply.err)
	assertResponsesCompatibilityReply(t, reply.response, "resp_refresh_repair", false)
	fixture.finish(t)
	requests := fixture.a.snapshot()
	require.Len(t, requests, 2)
	require.Equal(t, []string{"text/event-stream"}, requests[0].header.Values("Accept"))
	require.True(t, gjson.GetBytes(requests[0].body, "truncation").Exists())
	require.Equal(t, []string{"application/json"}, requests[1].header.Values("Accept"))
	require.False(t, gjson.GetBytes(requests[1].body, "truncation").Exists())
	require.Equal(t, "false", gjson.GetBytes(requests[1].body, "stream").Raw)
	require.Equal(t, responsesCompatibilityLuna, gjson.GetBytes(requests[1].body, "model").String())
	require.EqualValues(t, len(requests[1].body), requests[1].contentLength)
	require.Empty(t, fixture.b.snapshot())
}

// testResponsesRefreshSwitch revokes an otherwise eligible fallback after A
// has already received its converted request. The second candidate uses its
// own mapped body and observes global off before handoff.
func testResponsesRefreshSwitch(t *testing.T, db *sql.DB, pass bool) {
	service := responsecompat.New(settings.New(newResponsesRefreshRepository(db)))
	policy := responsesCompatibilityPolicy(responsecompat.Enforce)
	fallback := policy.Rules[0]
	fallback.ProviderID, fallback.UpstreamModel = responsesCompatibilityB, responsesCompatibilityFallbackModel
	policy.Rules = append(policy.Rules, fallback)
	on := saveResponsesRefreshPolicy(t, service, *policy)
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	knownRefusal := responsesCompatibilityPreExecutionRefusal(t)
	a := func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		select {
		case <-release:
			knownRefusal(w, r)
		case <-r.Context().Done():
		}
	}
	fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
		passthrough: pass, rewrite: true, compatibility: service, switchDirectly: true,
	}, a, responsesCompatibilitySuccess(t, "resp_refresh_switch", responsesCompatibilityFallbackModel, "false"))
	t.Cleanup(unblock)
	pending := startResponsesRefreshPost(t, fixture, refreshOmitted)
	receivedResponsesRefreshValue(t, received)
	on.Settings.Mode = responsecompat.Off
	off := saveResponsesRefreshPolicy(t, service, on.Settings)
	unblock()
	reply := receivedResponsesRefreshValue(t, pending)
	require.NoError(t, reply.err)
	assertResponsesCompatibilityReply(t, reply.response, "resp_refresh_switch", false)
	fixture.finish(t)
	assertResponsesCompatibilityWireRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, "true", "text/event-stream", responsesCompatibilityPath, pass)
	assertResponsesCompatibilityRequest(t, fixture.b.snapshot(), responsesCompatibilityB, responsesCompatibilityFallbackModel, "", pass)
	id := reply.response.header.Get("X-TokenRouter-Request-ID")
	assertResponsesRefreshCohort(t, db, id, on.Settings.Revision, responsecompat.Enforce, sql.NullInt64{Int64: off.Settings.Revision, Valid: true}, true)
	t.Logf("candidate switch: enrollment=%d final_evaluation=%d applied=true", on.Settings.Revision, off.Settings.Revision)
}

// testResponsesRefreshInFlight lets an already sent request finish in its
// converted mode, and separately leaves a sent body stalled until its existing
// compatibility clock closes the connection after rule withdrawal.
func testResponsesRefreshInFlight(t *testing.T, db *sql.DB, pass bool) {
	for _, stall := range []bool{false, true} {
		t.Run(fmt.Sprintf("stall_%t", stall), func(t *testing.T) {
			service := responsecompat.New(settings.New(newResponsesRefreshRepository(db)))
			on := saveResponsesRefreshPolicy(t, service, *responsesCompatibilityPolicy(responsecompat.Enforce))
			received := make(chan struct{}, 1)
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			closed := make(chan struct{})
			success := responsesCompatibilityCompletedStream(t, "resp_refresh_inflight", responsesCompatibilityLuna)
			a := func(w http.ResponseWriter, r *http.Request) {
				defer close(closed)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, ": ready\n\n")
				_ = http.NewResponseController(w).Flush()
				received <- struct{}{}
				select {
				case <-release:
					success(w, r)
				case <-r.Context().Done():
				}
			}
			fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
				passthrough: pass, rewrite: true, compatibility: service,
				total: 650 * time.Millisecond,
			}, a, responsesCompatibilityBadGateway(t))
			t.Cleanup(unblock)
			started := time.Now()
			pending := startResponsesRefreshPost(t, fixture, refreshExplicit)
			receivedResponsesRefreshValue(t, received)
			on.Settings.Mode = responsecompat.Off
			saveResponsesRefreshPolicy(t, service, on.Settings)
			if !stall {
				unblock()
			}
			reply := receivedResponsesRefreshValue(t, pending)
			require.NoError(t, reply.err)
			receivedResponsesRefreshValue(t, closed)
			t.Logf("in-flight handoff-to-closure bound from request start=%s", time.Since(started))
			if stall {
				require.Equal(t, http.StatusGatewayTimeout, reply.response.status, string(reply.response.body))
				require.Equal(t, "Upstream Responses request deadline exceeded", gjson.GetBytes(reply.response.body, "error.message").String())
				require.Less(t, time.Since(started), 2*time.Second)
			} else {
				assertResponsesCompatibilityReply(t, reply.response, "resp_refresh_inflight", false)
			}
			fixture.finish(t)
			assertResponsesCompatibilityWireRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, "true", "text/event-stream", responsesCompatibilityPath, pass)
			require.Empty(t, fixture.b.snapshot())
		})
	}
}

// newResponsesRefreshRepository creates a settings store on the migrated PostgreSQL primary.
func newResponsesRefreshRepository(db *sql.DB) settings.Repository {
	return settingspg.NewSettingRepository(ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db))))
}

// startResponsesRefreshPoller runs and joins one independent settings poller.
func startResponsesRefreshPoller(t *testing.T, service *responsecompat.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("settings poller did not stop")
		}
	})
}

// saveResponsesRefreshPolicy commits through production CAS using the saved revision.
func saveResponsesRefreshPolicy(t *testing.T, service *responsecompat.Service, config responsecompat.Config) responsecompat.Status {
	t.Helper()
	current, err := service.Get(t.Context())
	require.NoError(t, err)
	config.Revision = current.Settings.Revision
	status, err := service.Update(t.Context(), config)
	require.NoError(t, err)
	require.Equal(t, config.Revision+1, status.Settings.Revision)
	require.NotNil(t, status.AppliedRevision)
	require.Equal(t, status.Settings.Revision, *status.AppliedRevision)
	return status
}

// assertResponsesRefreshCohort verifies immutable membership and the version
// observed by the last eligible send evaluation after transport finalizes.
func assertResponsesRefreshCohort(t *testing.T, db *sql.DB, id string, revision int64, arm responsecompat.Mode, finalRevision sql.NullInt64, applied bool) {
	t.Helper()
	var seenRevision int64
	var seenArm string
	var seenFinalRevision sql.NullInt64
	var group, provider int64
	var model, endpoint string
	var outcome sql.NullString
	var seenApplied, seenFinished bool
	err := db.QueryRowContext(t.Context(), `SELECT revision, arm, applied, finished_at IS NOT NULL, final_revision, group_id, provider_id, model, endpoint, outcome
		FROM gateway_compatibility_cohorts WHERE phase=$1 AND request_id=$2`, responsecompat.ResponsesPhase, id).
		Scan(&seenRevision, &seenArm, &seenApplied, &seenFinished, &seenFinalRevision, &group, &provider, &model, &endpoint, &outcome)
	require.NoError(t, err)
	require.Equal(t, revision, seenRevision)
	require.Equal(t, string(arm), seenArm)
	require.Equal(t, finalRevision, seenFinalRevision)
	require.Equal(t, finalRevision.Valid, seenFinished)
	require.Equal(t, applied, seenApplied)
	require.EqualValues(t, responsesCompatibilityGroup, group)
	require.EqualValues(t, responsesCompatibilityA, provider)
	require.Equal(t, responsesCompatibilityLuna, model)
	require.Equal(t, responsesCompatibilityPath, endpoint)
	if finalRevision.Valid {
		require.Equal(t, sql.NullString{String: responsecompat.OutcomeCompleted, Valid: true}, outcome)
	}
}

// startResponsesRefreshPost submits one real HTTP request until EOF or cancellation.
func startResponsesRefreshPost(t *testing.T, fixture *responsesCompatibilityFixture, payload string) <-chan refreshReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), responsesCompatibilityTimeout)
	t.Cleanup(cancel)
	result := make(chan refreshReply, 1)
	go func() {
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gateway.URL+responsesCompatibilityPath, strings.NewReader(payload))
		if err != nil {
			result <- refreshReply{err: err}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		res, err := fixture.gateway.Client().Do(req)
		if err != nil {
			result <- refreshReply{err: err}
			return
		}
		body, err := io.ReadAll(res.Body)
		err = errors.Join(err, res.Body.Close())
		result <- refreshReply{response: responsesCompatibilityReply{status: res.StatusCode, header: res.Header.Clone(), body: body}, err: err}
	}()
	return result
}

// receivedResponsesRefreshValue bounds test synchronization on a real service.
func receivedResponsesRefreshValue[T any](t *testing.T, from <-chan T) T {
	t.Helper()
	select {
	case value := <-from:
		return value
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for refresh scenario")
		var zero T
		return zero
	}
}

// DoWithTLS observes the final prepared request and delegates to the real HTTP client.
func (p responsesCompatibilityTransportTap) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	p.inspect(req)
	return p.UpstreamTransport.DoWithTLS(req, proxy, id, concurrency, profile)
}

// GetValue reads the primary, optionally inducing a query timeout, a database
// error or an already captured revision returning out of order. The late read
// intentionally waits after context cancellation to test rejection at publication.
func (r *responsesRefreshRepository) GetValue(ctx context.Context, key string) (string, error) {
	var value string
	if r.slow.Load() {
		started := time.Now()
		err := r.db.QueryRowContext(ctx, `SELECT value FROM settings CROSS JOIN (SELECT pg_sleep(2)) AS delayed WHERE key=$1`, key).Scan(&value)
		select {
		case r.firstSlow <- refreshRead{started: started, elapsed: time.Since(started), err: ctx.Err()}:
		default:
		}
		return value, err
	}
	if r.fail.Load() {
		err := r.db.QueryRowContext(ctx, `SELECT value FROM responses_refresh_unavailable_settings WHERE key=$1`, key).Scan(&value)
		return value, err
	}
	value, err := r.Repository.GetValue(ctx, key)
	if ctx.Value(refreshDelayReadKey{}) != nil {
		r.captured <- time.Now()
		<-r.release
	}
	return value, err
}

// IncrementProviderWaitCount signals the real HTTP provider queue registration.
func (s *responsesRefreshSlots) IncrementProviderWaitCount(context.Context, int64, int) (bool, error) {
	s.queued <- struct{}{}
	return true, nil
}
