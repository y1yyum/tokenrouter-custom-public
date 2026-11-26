//go:build integration

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/billing/pricing"
	"github.com/TokenFlux/TokenRouter/internal/gateway/completion"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/requestlog"
	requestlogpg "github.com/TokenFlux/TokenRouter/internal/requestlog/postgres"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// gateway_responses_settlement_scenario_integration_test.go covers the real
// conversion, request record, and settlement path for both Responses builders.
// Apply probes count invocations before SQL dedup can hide a second attempt.

// These frames make terminal-only metering distinguishable from corroborated usage.
const (
	settlementPayload     = `{"model":"gpt-5.4","input":"return ok","max_tokens":20,"stream":false,"service_tier":"priority"}`
	settlementUsage       = `"usage":{"input_tokens":7,"output_tokens":2}`
	settlementIndependent = `{"type":"response.usage","response_id":"resp_settle","service_tier":"default",` + settlementUsage + `}`
	settlementImage       = `{"type":"response.output_item.done","response_id":"resp_settle","output_index":0,"item":{"id":"img_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo="}}`
	settlementSearch      = `{"type":"response.output_item.done","response_id":"resp_settle","output_index":0,"item":{"id":"search_1","type":"web_search_call","status":"completed"}}`
	settlementFailure     = `{"type":"response.failed","response":{"id":"resp_settle","status":"failed","error":{"code":"server_is_overloaded","message":"busy","execution_started":true}}}`
	settlementCompleted   = `{"type":"response.completed","response":{"id":"resp_settle","status":"completed","service_tier":"default","output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],` + settlementUsage + `}}`
	settlementIncomplete  = `{"type":"response.incomplete","response":{"id":"resp_settle","status":"incomplete","service_tier":"default","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],` + settlementUsage + `}}`
)

// responsesSettlementCatalog keeps step 03 token rates and adds a known image rate.
type responsesSettlementCatalog struct{ streamPriceCatalog }

// TestGatewayResponsesCompatibilitySettlement verifies persisted final evidence and
// the real funding boundary for both ordinary HTTP and rewrite passthrough.
func TestGatewayResponsesCompatibilitySettlement(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	conflictingTerminal := strings.Replace(settlementCompleted, `"response":`, `"usage":{"input_tokens":70,"output_tokens":2},"response":`, 1)
	conflictingTerminal = strings.Replace(conflictingTerminal, `],`+settlementUsage, `],"usage":{"input_tokens":17,"output_tokens":2}`, 1)
	aliasConflict := strings.Replace(settlementCompleted, `"input_tokens":7`, `"input_tokens":7,"prompt_tokens":70`, 1)
	outerZero := strings.Replace(settlementCompleted, `"response":`, `"usage":{"input_tokens":0,"output_tokens":0},"response":`, 1)
	innerZero := strings.Replace(outerZero, `"input_tokens":0,"output_tokens":0`, `"input_tokens":7,"output_tokens":2`, 1)
	innerZero = strings.Replace(innerZero, `],`+settlementUsage, `],"usage":{"input_tokens":0,"output_tokens":0}`, 1)
	sparse := strings.Replace(settlementCompleted, `"response":`, `"usage":{"input_tokens":7},"response":`, 1)
	sparse = strings.Replace(sparse, `],`+settlementUsage, `],"usage":{"output_tokens":2}`, 1)
	badTail := `{"type":"response.in_progress","response_id":"resp_settle","usage":{"input_tokens":70,"output_tokens":2}}`
	badTierTail := `{"type":"response.usage","response_id":"resp_settle","service_tier":"priority","usage":{"input_tokens":70,"output_tokens":2}}`
	conflictingProgress := strings.Replace(settlementIndependent, `"input_tokens":7`, `"input_tokens":7,"prompt_tokens":70`, 1)
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough_%t", passthrough), func(t *testing.T) {
			for _, tc := range []struct {
				name, disposition, evidence, source, resolution string
				frames                                          []string
				status, complete, images                        int
				cost                                            float64
				fullQueue                                       bool
				stall                                           bool
			}{
				{name: "failed_tokens_queue_full", frames: []string{settlementIndependent, settlementFailure}, status: 503, complete: 1, cost: .11, fullQueue: true, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "failed"},
				{name: "read_timeout_with_tokens", frames: []string{settlementIndependent}, status: 504, complete: 1, cost: .11, stall: true, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "protocol_error"},
				{name: "failed_image_without_tokens", frames: []string{settlementImage, settlementFailure}, status: 503, complete: 1, cost: .37, images: 1, disposition: "chargeable", evidence: "missing", source: "image_output", resolution: "failed"},
				{name: "failed_search_without_tokens", frames: []string{settlementSearch, settlementSearch, settlementFailure}, status: 503, complete: 1, cost: .19, disposition: "chargeable", evidence: "missing", source: "operation", resolution: "failed"},
				{name: "zero", frames: []string{`{"type":"response.usage","response_id":"resp_settle","usage":{"input_tokens":0,"output_tokens":0}}`, settlementFailure}, status: 503, complete: 1, disposition: "zero", evidence: "zero", source: "independent", resolution: "failed"},
				{name: "missing", frames: []string{settlementFailure}, status: 503, complete: 1, disposition: "pending_review", evidence: "missing", resolution: "failed"},
				{name: "negative", frames: []string{`{"type":"response.usage","response_id":"resp_settle","usage":{"input_tokens":-3,"output_tokens":2}}`, settlementFailure}, status: 503, complete: 1, disposition: "pending_review", evidence: "unverified", source: "independent", resolution: "failed"},
				{name: "contradiction_terminal_only", frames: []string{settlementIncomplete, settlementCompleted}, status: 502, complete: 1, disposition: "pending_review", evidence: "unverified", source: "terminal", resolution: "protocol_error"},
				{name: "contradiction_independent", frames: []string{settlementIndependent, settlementIncomplete, strings.Replace(settlementCompleted, `"default"`, `"priority"`, 1)}, status: 502, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "protocol_error"},
				{name: "contradiction_independent_tier_unknown", frames: []string{settlementIndependent, strings.Replace(settlementIndependent, `"default"`, `"flex"`, 1), settlementIncomplete, settlementCompleted}, status: 502, complete: 1, disposition: "pending_review", evidence: "unverified", source: "independent", resolution: "protocol_error"},
				{name: "same_frame_views_conflict", frames: []string{conflictingTerminal}, status: 502, complete: 1, disposition: "pending_review", evidence: "unverified", source: "terminal", resolution: "protocol_error"},
				{name: "same_frame_aliases_conflict", frames: []string{aliasConflict}, status: 502, complete: 1, disposition: "pending_review", evidence: "unverified", source: "terminal", resolution: "protocol_error"},
				{name: "same_frame_conflict_with_prior_independent_usage", frames: []string{settlementIndependent, conflictingTerminal}, status: 502, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "protocol_error"},
				{name: "same_frame_conflict_with_later_independent_usage", frames: []string{conflictingTerminal, settlementIndependent}, status: 502, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "protocol_error"},
				{name: "same_frame_outer_zero", frames: []string{outerZero}, status: 200, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "terminal", resolution: "completed"},
				{name: "same_frame_inner_zero", frames: []string{innerZero}, status: 200, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "terminal", resolution: "completed"},
				{name: "same_frame_sparse", frames: []string{sparse}, status: 200, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "terminal", resolution: "completed"},
				{name: "same_frame_progress_conflict_blocks_replay", frames: []string{conflictingProgress, strings.Replace(settlementFailure, `"execution_started":true`, `"execution_started":false`, 1)}, status: 503, complete: 1, disposition: "pending_review", evidence: "unverified", source: "independent", resolution: "failed"},
				{name: "same_frame_progress_conflict_with_valid_terminal", frames: []string{conflictingProgress, settlementCompleted}, status: 200, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "terminal", resolution: "completed"},
				{name: "illegal_tail_only", frames: []string{settlementCompleted, badTail}, status: 502, complete: 1, disposition: "pending_review", evidence: "unverified", source: "terminal", resolution: "protocol_error"},
				{name: "illegal_tail_after_prior_independent_usage", frames: []string{settlementIndependent, settlementCompleted, badTail}, status: 502, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "protocol_error"},
				{name: "independent_usage_after_illegal_tail", frames: []string{settlementCompleted, badTail, settlementIndependent}, status: 502, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "protocol_error"},
				{name: "independent_tier_after_rejected_terminal", frames: []string{strings.Replace(settlementCompleted, `"default"`, `"priority"`, 1), badTail, settlementIndependent}, status: 502, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "protocol_error"},
				{name: "illegal_tail_cannot_bind_anonymous_usage", frames: []string{strings.Replace(settlementIndependent, `"response_id":"resp_settle",`, "", 1), `{"type":"error","code":"server_is_overloaded","message":"busy","execution_started":true}`, badTail}, status: 502, complete: 1, disposition: "pending_review", evidence: "missing", resolution: "protocol_error"},
				{name: "illegal_tier_tail_preserves_prior_independent_usage", frames: []string{settlementIndependent, settlementCompleted, badTierTail}, status: 502, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "independent", resolution: "protocol_error"},
				{name: "duplicate_incomplete", frames: []string{settlementIncomplete, settlementIncomplete}, status: 200, complete: 1, cost: .11, disposition: "chargeable", evidence: "reported", source: "terminal", resolution: "incomplete"},
				{name: "no_transport_result", status: 502, disposition: "pending_review", evidence: "missing", resolution: "no_result"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					records := requestlog.NewService(requestlogpg.NewStore(db), 7, t.Logf)
					options := responsesCompatibilityFixtureOptions{
						passthrough: passthrough, rewrite: passthrough,
						policy: responsesCompatibilityPolicy(responsecompat.Enforce), observeRequest: records.Observe, catalog: responsesSettlementCatalog{},
					}
					if tc.fullQueue {
						options.worker = fullResponsesSettlementQueue(t)
					}
					if tc.stall {
						options.transport = openai.ResponsesTransportOptions{FirstOutputTimeout: 2 * time.Second, StreamInterval: 300 * time.Millisecond}
					}
					fixture := newResponsesCompatibilityConfiguredFixture(t, db, options, responsesSettlementStream(t, tc.frames, tc.stall), responsesCompatibilityBadGateway(t))
					before := readResponsesCompatibilityLedger(t, db)
					reply := fixture.post(t, settlementPayload)
					fixture.finish(t)
					if tc.fullQueue {
						require.EqualValues(t, 1, options.worker.Stats().DroppedQueueFull)
					}
					require.NoError(t, records.Flush(t.Context()))
					assertResponsesCompatibilityWireRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, "true", "text/event-stream", responsesCompatibilityPath, passthrough)
					require.Equal(t, "priority", gjson.GetBytes(fixture.a.snapshot()[0].body, "service_tier").String())
					require.Empty(t, fixture.b.snapshot())
					require.Equal(t, tc.status, reply.status, "%s", reply.body)
					require.True(t, json.Valid(reply.body), "%s", reply.body)
					require.Contains(t, reply.header.Get("Content-Type"), "application/json")
					require.NotContains(t, string(reply.body), "data: ")
					if tc.status >= 400 {
						require.True(t, gjson.GetBytes(reply.body, "error").Exists())
					} else {
						require.EqualValues(t, 7, gjson.GetBytes(reply.body, "usage.input_tokens").Int())
						require.EqualValues(t, 2, gjson.GetBytes(reply.body, "usage.output_tokens").Int())
					}
					id := reply.header.Get("X-TokenRouter-Request-ID")
					saved := readResponsesSettlementRequest(t, db, id)
					final := saved.ResponsesSettlement
					require.Equal(t, tc.disposition, final.Disposition)
					require.Equal(t, tc.evidence, final.Evidence)
					if tc.source != "" {
						require.Equal(t, tc.source, final.Source)
					}
					require.Equal(t, tc.resolution, final.Resolution)
					require.Equal(t, tc.complete, final.CompletionCount)
					require.Equal(t, tc.status, saved.Status)
					require.EqualValues(t, responsesCompatibilityA, final.ProviderID)
					assertResponsesSettlementAmount(t, db, fixture, id, before, tc.cost, tc.images, tc.evidence == "reported")
				})
			}
			t.Run("client_disconnect_with_trailing_usage", func(t *testing.T) {
				assertResponsesSettlementDisconnect(t, db, passthrough)
			})
		})
	}
}

// TestGatewayResponsesPreparedSettlementReplay sends the same prepared native
// command through the worker twice, then sends a different digest for that key.
// SQL must retain one usage row, one claim, and one balance movement.
func TestGatewayResponsesPreparedSettlementReplay(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	fixture := newResponsesCompatibilityFixture(t, db, false, responsesCompatibilityBadGateway(t), responsesCompatibilityBadGateway(t))
	before := readResponsesCompatibilityLedger(t, db)
	group := int64(responsesCompatibilityGroup)
	input := &completion.Input{
		BillingKey: "client:compat-prepared-native", RequestID: "compat-prepared-native", RequestPayloadHash: "original-digest",
		APIKey:   &completion.KeySnapshot{ID: responsesCompatibilityKey, GroupID: &group, BillingMode: "balance", Group: &completion.GroupSnapshot{ID: group, RateMultiplier: 1}},
		User:     &completion.PayerSnapshot{ID: responsesCompatibilityUser},
		Provider: &completion.ProviderSnapshot{ID: responsesCompatibilityA, Type: "apikey", Platform: "openai", RateMultiplier: 1},
		Result: &completion.Result{
			NativeUsage: true, Model: responsesCompatibilityLuna, UpstreamModel: responsesCompatibilityLuna,
			Usage: completion.TokenUsage{InputTokens: 7, OutputTokens: 2, CacheReadInputTokens: 3},
		},
	}
	original, err := fixture.recorder.PrepareOpenAI(t.Context(), input)
	require.NoError(t, err)
	input.RequestPayloadHash = "different-digest"
	conflict, err := fixture.recorder.PrepareOpenAI(t.Context(), input)
	require.NoError(t, err)
	// A mutable caller retaining this input cannot change either command after preparation.
	input.Result.Usage.InputTokens = 99
	pool := completion.NewUsageRecordWorkerPoolWithOptions(completion.UsageRecordWorkerPoolOptions{WorkerCount: 1})
	t.Cleanup(pool.Stop)
	errs := make(chan error, 3)
	for _, commit := range []func(context.Context) error{original, original, conflict} {
		require.Equal(t, completion.UsageRecordSubmitModeEnqueued, pool.Submit(func(ctx context.Context) { errs <- commit(ctx) }))
	}
	pool.Stop()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.ErrorIs(t, <-errs, billing.ErrUsageBillingRequestConflict)
	require.Len(t, fixture.funds.seen, 3)
	first, replay, different := <-fixture.funds.seen, <-fixture.funds.seen, <-fixture.funds.seen
	require.Equal(t, first, replay)
	require.Equal(t, first.RequestID, different.RequestID)
	require.NotEqual(t, first.RequestFingerprint, different.RequestFingerprint)
	require.Equal(t, []int{7, 2, 3}, []int{first.InputTokens, first.OutputTokens, first.CacheReadTokens})
	require.Equal(t, "original-digest", first.RequestPayloadHash)
	require.InDelta(t, .119, first.BillableAmountUSD, 1e-8)
	after := readResponsesCompatibilityLedger(t, db)
	require.Equal(t, 1, after.usage-before.usage)
	require.Equal(t, 1, after.claims-before.claims)
	require.InDelta(t, .119, before.balance-after.balance, 1e-8)
	var fingerprint string
	var actual float64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT d.request_fingerprint,u.actual_cost
FROM usage_logs u JOIN usage_billing_dedup d ON d.request_id=u.billing_key AND d.api_key_id=u.api_key_id WHERE u.request_id=$1`, input.RequestID).
		Scan(&fingerprint, &actual))
	require.Equal(t, first.RequestFingerprint, fingerprint)
	require.InDelta(t, .119, actual, 1e-8)
	t.Logf("native replay Apply=3 usage=1 claims=1 wallet_delta=%.3f fingerprint_conflicts=1", before.balance-after.balance)
}

// GetModelPricing preserves distinct step 03 bucket rates and prices one image.
func (catalog responsesSettlementCatalog) GetModelPricing(model string) *pricing.CatalogModelPricing {
	price := catalog.streamPriceCatalog.GetModelPricing(model)
	price.OutputCostPerImage = .37
	price.ImagePricePresent = true
	price.SupportsServiceTier = true
	price.InputCostPerTokenPriority = .05
	price.OutputCostPerTokenPriority = .10
	return price
}

// BillingDefaults supplies a distinct charge for each completed search call.
func (responsesSettlementCatalog) BillingDefaults() pricing.OperationPrices {
	return pricing.OperationPrices{SearchPricePer1k: new(190.0)}
}

// fullResponsesSettlementQueue parks the only worker and occupies the only wait
// slot. Assertions run before cleanup unblocks the queue.
func fullResponsesSettlementQueue(t *testing.T) *completion.UsageRecordWorkerPool {
	t.Helper()
	pool := completion.NewUsageRecordWorkerPoolWithOptions(completion.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1, OverflowPolicy: "drop", TaskTimeout: 30 * time.Second})
	started, release := make(chan struct{}), make(chan struct{})
	releaseWorker := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		releaseWorker()
		pool.Stop()
	})
	require.Equal(t, completion.UsageRecordSubmitModeEnqueued, pool.Submit(func(ctx context.Context) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}))
	select {
	case <-started:
	case <-t.Context().Done():
		t.Fatal("completion worker never started")
	}
	require.Equal(t, completion.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {}))
	return pool
}

// responsesSettlementStream emits a strict buffered stream or closes the
// transport without returning headers or a reader result.
func responsesSettlementStream(t *testing.T, frames []string, stall bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if len(frames) == 0 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				err = conn.Close()
			}
			if err != nil {
				t.Errorf("close fixture upstream: %v", err)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "fixture-settle")
		for _, frame := range frames {
			if _, err := io.WriteString(w, "data: "+frame+"\n\n"); err != nil {
				t.Errorf("write settlement frame: %v", err)
				return
			}
		}
		if stall {
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush settlement frame: %v", err)
				return
			}
			<-r.Context().Done()
		}
	}
}

// assertResponsesSettlementDisconnect cancels the actual HTTP client after
// the first upstream frame has arrived. Usage is sent after the server observes
// the cancellation, so settlement must use the bounded drain and detached context.
func assertResponsesSettlementDisconnect(t *testing.T, db *sql.DB, passthrough bool) {
	t.Helper()
	seen := make(chan string, 1)
	disconnected := make(chan struct{})
	var seenOnce, disconnectOnce sync.Once
	records := requestlog.NewService(requestlogpg.NewStore(db), 7, t.Logf)
	observe := func(record telemetry.RequestRecord) {
		records.Observe(record)
		for _, alias := range record.Aliases {
			if alias.Kind == "response" && alias.Value == "resp_settle" {
				seenOnce.Do(func() { seen <- record.RequestID })
			}
		}
		if record.ClientDisconnectedAt != nil {
			disconnectOnce.Do(func() { close(disconnected) })
		}
	}
	a := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_settle","status":"in_progress","output":[]}}`+"\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush upstream preamble: %v", err)
			return
		}
		select {
		case <-disconnected:
			_, _ = io.WriteString(w, "data: "+settlementIndependent+"\n\ndata: "+settlementFailure+"\n\n")
		case <-r.Context().Done():
			t.Error("upstream closed before drain")
		}
	})
	fixture := newResponsesCompatibilityConfiguredFixture(t, db, responsesCompatibilityFixtureOptions{
		passthrough: passthrough, rewrite: passthrough, policy: responsesCompatibilityPolicy(responsecompat.Enforce),
		observeRequest: observe, catalog: responsesSettlementCatalog{},
	}, a, responsesCompatibilityBadGateway(t))
	before := readResponsesCompatibilityLedger(t, db)
	ctx, cancel := context.WithTimeout(t.Context(), responsesCompatibilityTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fixture.gateway.URL+responsesCompatibilityPath, strings.NewReader(settlementPayload))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		response, err := fixture.gateway.Client().Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		done <- err
	}()
	var id string
	select {
	case id = <-seen:
	case <-ctx.Done():
		t.Fatal("gateway did not observe the upstream preamble")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	fixture.finish(t)
	require.NoError(t, records.Flush(t.Context()))
	saved := readResponsesSettlementRequest(t, db, id)
	require.Equal(t, "canceled", saved.State)
	require.Equal(t, "client_gone", saved.ResponsesSettlement.Delivery)
	require.Equal(t, "chargeable", saved.ResponsesSettlement.Disposition)
	require.Equal(t, "independent", saved.ResponsesSettlement.Source)
	require.Equal(t, 1, saved.ResponsesSettlement.CompletionCount)
	require.Len(t, fixture.a.snapshot(), 1)
	require.Empty(t, fixture.b.snapshot())
	assertResponsesSettlementAmount(t, db, fixture, id, before, .11, 0, true)
}

// readResponsesSettlementRequest loads the persisted request independently of usage.
func readResponsesSettlementRequest(t *testing.T, db *sql.DB, id string) telemetry.RequestRecord {
	t.Helper()
	var record []byte
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT record FROM request_records WHERE request_id=$1", id).Scan(&record))
	var saved telemetry.RequestRecord
	require.NoError(t, json.Unmarshal(record, &saved))
	return saved
}

// assertResponsesSettlementAmount checks the independent row counts and wallet
// delta before verifying the persisted identity, tier, and amount of a charge.
func assertResponsesSettlementAmount(t *testing.T, db *sql.DB, fixture *responsesCompatibilityFixture, id string, before responsesCompatibilityLedger, cost float64, images int, tokens bool) {
	t.Helper()
	after := readResponsesCompatibilityLedger(t, db)
	if cost == 0 {
		require.Empty(t, fixture.funds.seen, "Apply calls")
		require.Equal(t, before, after)
		t.Log("Apply=0 usage=0 claims=0 wallet_delta=0")
		return
	}
	require.Len(t, fixture.funds.seen, 1, "Apply calls")
	cmd := <-fixture.funds.seen
	require.Equal(t, 1, after.usage-before.usage)
	require.Equal(t, 1, after.claims-before.claims)
	require.InDelta(t, cost, before.balance-after.balance, 1e-8)
	var tier, fingerprint, key, model string
	var imageCount, input, output int
	var actual float64
	var stream bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COALESCE(u.service_tier,''),d.request_fingerprint,u.billing_key,u.upstream_model,u.image_count,u.input_tokens,u.output_tokens,u.actual_cost,u.stream
FROM usage_logs u JOIN usage_billing_dedup d ON d.request_id=u.billing_key AND d.api_key_id=u.api_key_id WHERE u.request_id=$1`, id).
		Scan(&tier, &fingerprint, &key, &model, &imageCount, &input, &output, &actual, &stream))
	require.Equal(t, "client:"+id, key)
	require.Equal(t, billing.HashUsageRequestPayload([]byte(settlementPayload)), cmd.RequestPayloadHash)
	require.NotEmpty(t, fingerprint)
	require.Equal(t, cmd.RequestFingerprint, fingerprint)
	require.Equal(t, responsesCompatibilityLuna, model)
	require.Equal(t, images, imageCount)
	require.False(t, stream)
	require.InDelta(t, cost, actual, 1e-8)
	require.InDelta(t, cost, cmd.BillableAmountUSD, 1e-8)
	if tokens {
		require.Equal(t, "default", tier)
		require.Equal(t, tier, cmd.ServiceTier)
		require.Equal(t, []int{7, 2}, []int{input, output})
		require.Equal(t, input, cmd.InputTokens)
		require.Equal(t, output, cmd.OutputTokens)
	} else {
		require.Zero(t, input)
		require.Zero(t, output)
	}
	t.Logf("Apply=1 usage=1 claims=1 wallet_delta=%.2f tier=%s images=%d", before.balance-after.balance, tier, images)
}
