package openaiattempt

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/egress"
	"github.com/TokenFlux/TokenRouter/internal/gateway/completion"
	"github.com/TokenFlux/TokenRouter/internal/gateway/failover"
	forwardcore "github.com/TokenFlux/TokenRouter/internal/gateway/forward"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewaycapture "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	textflow "github.com/TokenFlux/TokenRouter/internal/gateway/text"
	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry/logging"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry/timing"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	providercore "github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/routing/capability"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/server/clientip"
)

// responses_attempts.go manages Responses provider attempts and qualifies
// their retry and completion outcomes.
// Responses attempts retain canonical request input while providers, resource
// callbacks, and observed results change through a serial text loop. A selected
// HTTP continuation target needs eligible native Responses API-key credentials.
// Unsuitable routes release acquired capacity and return a skip result; the
// selector keeps an immutable continuation bound to its originating provider.
//
// Forwarding releases provider capacity before recording Cyber effects and
// exposing its outcome. Adjusted output size distinguishes registered
// keepalives from meaningful response bytes, so committed headers can coexist
// with an open retry window. Native usage and produced images can support
// partial completion, while retry eligibility still follows classified errors,
// client cancellation, and output recovery evidence. Completion captures
// request/result values before submitting work that can run after HTTP handling
// finishes.

// errResponsesSelectionEmpty identifies an empty selection so the handler can distinguish routing capacity from model availability.
var errResponsesSelectionEmpty = errors.New("no provider returned for Responses selection")

// responsesAttemptBridge retains admitted request identity and current
// provider-attempt state. The serial text loop changes provider resources and
// results while shared runtime services remain bound through fixed.
type responsesAttemptBridge struct {
	// optional request session registrations
	sessionAttempts *scheduler.SessionAttempts
	// known sticky provider binding
	hasBoundSession bool
	// request retry cache-billing intent
	forceCacheBilling bool
	// consumed destination fallback opportunity
	groupFallbackUsed bool
	// shared execution service bindings
	fixed *openAIExecutionDependencies
	// active HTTP request context
	c *gin.Context
	// current effective funding key
	apiKey *apikey.APIKey
	// original authenticated caller identity
	subject authctx.AuthSubject
	// current effective funding subscription
	subscription *billing.UserSubscription
	// request attempt diagnostic logger
	reqLog *zap.Logger
	// body, forwardBody, and sessionHashBody retain one request's source
	// representations. The serial loop derives provider input from
	// forwardBody and hashes body for completion; sessionHashBody supplies
	// Cyber session matching when marked.
	// body: admitted client request bytes. forwardBody: planned
	// group-mapped input bytes.
	// sessionHashBody: retained session matching input.
	body, forwardBody, sessionHashBody []byte
	// reqModel, forwardModel, sessionHash, previousResponseID, and
	// requestPlatform describe one admitted request. Group fallback changes
	// forwarding identity while the client model and continuation remain
	// available for diagnostics and selection.
	// reqModel: original requested model. forwardModel: current
	// group-mapped model.
	// sessionHash: active sticky selection seed. previousResponseID:
	// supplied continuation response ID.
	// requestPlatform: optional provider-platform selection filter.
	reqModel, forwardModel, sessionHash, previousResponseID, requestPlatform string
	// reqStream, nativeCompactionV2, legacyCompact, and requireCompact
	// retain admission flags through serial selection and forwarding. They
	// select output, compaction accounting, and provider capability checks
	// for the same HTTP request.
	// reqStream: client streaming request flag. nativeCompactionV2: native
	// compaction accounting flag.
	// legacyCompact: legacy Compact endpoint flag. requireCompact: required
	// provider compaction capability.
	reqStream, nativeCompactionV2, legacyCompact, requireCompact bool
	// streamStarted is shared with slot waits and terminal output. The
	// output adapter supplies a nonnil pointer that remains valid through
	// the serial request loop.
	streamStarted *bool
	// selectionCtx holds the current routing and billing context. Begin,
	// PrepareRetry, and group fallback publish updates before the next
	// selection reads it.
	selectionCtx context.Context
	// current admitted group mapping
	groupMapping routing.GroupMappingResult
	// request routing phase start
	routingStart time.Time
	// selected endpoint capability requirement
	requiredCapability providercore.OpenAIEndpointCapability
	// selection retains current scheduler target and acquired/wait
	// metadata. Select replaces it before Acquire adopts its provider
	// capacity and resource callback.
	selection *gatewaycapture.SelectionResult
	// provider retains the current execution target for forwarding and
	// synchronous completion capture. A subsequent Select replaces it for
	// the next serial attempt.
	provider *gatewaycapture.ExecutionProvider
	// providerReleaseFunc owns acquired provider cleanup. Forward invokes
	// it before policy/latency processing; the shared slot adapter
	// determines cancellation versus completion release.
	providerReleaseFunc func()
	// result retains the most recent forwarding observation, including
	// usage- bearing partial failures. Complete reads it only after the
	// text loop chooses completion handling.
	result *forwardcore.OpenAIResult
	// completionCalled guards the logical request's single completion handoff.
	completionCalled bool
	// settlementSeen retains pilot participation across a permitted pre-execution retry.
	settlementSeen bool
	// settlementFailed prevents a metered forwarding error from resetting provider protection.
	settlementFailed bool
	// settlementFinal accumulates the final observation before request-log persistence.
	settlementFinal telemetry.ResponsesSettlementObservation
	// settlementPricingAt fixes time-based pricing to the final forwarding attempt.
	settlementPricingAt time.Time
	// writerSizeBeforeForward measures adjusted output before this
	// exchange. Heartbeat subtraction makes RetryReady compare
	// meaningful bytes rather than committed keepalive traffic.
	writerSizeBeforeForward int
	// cyberPolicyHandled and wroteFallback retain different terminal
	// effects for this request. Cyber handling is passed to completion
	// capture, while fallback emission controls failure logging after
	// protocol output handling.
	// cyberPolicyHandled: current Cyber mark handled. wroteFallback:
	// missing terminal error supplied.
	cyberPolicyHandled, wroteFallback bool
	// retained cross-mode reasoning history
	passthroughFailoverState openAIPassthroughFailoverState
	// current terminal-failure diagnostic attributes
	fields []zap.Field
}

// Select resolves the current request under transport, capability, and
// continuation constraints. Empty selections and errors return to the text
// loop for terminal handling alongside any preceding upstream failure. A selected
// target is retained for acquisition, forwarding, scheduling observations, and
// cleanup.
//
// HTTP previous_response_id requires an eligible native Responses API-key
// route. Other selected routes release any selection callback and return
// a classified Skip outcome. Selection retains the originating provider for
// immutable HTTP continuations, so a rejected binding cannot send its response
// reference through another credential. Eligible targets receive a retained
// pool-session seed and selected-provider observations.
func (b *responsesAttemptBridge) Select(excluded map[int64]struct{}) (textflow.ResponseSelection, error) {
	b.reqLog.Debug("openai.provider_selecting", zap.Int("excluded_provider_count", len(excluded)))
	var scheduleDecision scheduler.PlatformDecision
	var err error
	selectStart := time.Now()
	b.selection, scheduleDecision, err = b.binding().selectProviderWithSchedulerForCapability(
		b.selectionCtx,
		b.apiKey.GroupID,
		b.previousResponseID,
		b.sessionHash,
		b.reqModel,
		excluded, egress.OpenAIUpstreamTransportAny, b.requiredCapability,
		b.requireCompact,
		false,
		b.requestPlatform,
	)
	if b.c != nil && b.c.Request != nil {
		timing.ObserveSubstage(b.c.Request.Context(), "scheduler_select_ms", time.Since(selectStart))
	}
	if err != nil {
		return textflow.ResponseSelection{}, err
	}
	if b.selection == nil || b.selection.Provider == nil {
		return textflow.ResponseSelection{}, errResponsesSelectionEmpty
	}
	if b.previousResponseID != "" && b.selection != nil && b.selection.Provider != nil {
		b.reqLog.Debug("openai.provider_selected_with_previous_response_id", zap.Int64("provider_id", b.selection.Provider.Record.ID))
	}
	b.reqLog.Debug("openai.provider_schedule_decision",
		zap.String("layer", scheduleDecision.Layer),
		zap.Bool("sticky_previous_hit", scheduleDecision.StickyPreviousHit),
		zap.Bool("sticky_session_hit", scheduleDecision.StickySessionHit),
		zap.Int("candidate_count", scheduleDecision.CandidateCount),
		zap.Int("top_k", scheduleDecision.TopK),
		zap.Int64("latency_ms", scheduleDecision.LatencyMs),
		zap.Float64("load_skew", scheduleDecision.LoadSkew),
	)
	b.provider = b.selection.Provider
	if b.previousResponseID != "" && !gatewaycapture.ExecutionProtocolTarget(b.provider).SupportsHTTPResponsesContinuation() {
		// Continuation references require an eligible native Responses route.
		// Release the rejected target so failed admission cannot retain provider capacity.
		if b.selection.ReleaseFunc != nil {
			b.selection.ReleaseFunc()
			b.selection.ReleaseFunc = nil
		}
		skipped := &forwardcore.UpstreamFailoverError{
			StatusCode:       http.StatusBadRequest,
			Stage:            forwardcore.GatewayFailureStageInference,
			Scope:            forwardcore.GatewayFailureScopeRequest,
			Reason:           forwardcore.OpenAIHTTPContinuationUnsupportedReason,
			ClientStatusCode: http.StatusBadRequest,
			ClientMessage:    "previous_response_id requires a native Responses API-key provider for HTTP requests",
		}
		b.reqLog.Debug("openai.provider_skipped_http_continuation_unsupported",
			zap.Int64("provider_id", b.provider.Record.ID),
			zap.String("provider_type", b.provider.Record.Type),
		)
		picked := b.selectedView()
		picked.Skip = &textflow.AttemptFailure{Cause: skipped, Policy: skipped.RetryFailure()}
		return picked, nil
	}
	b.sessionHash = EnsureOpenAIPoolModeSessionHash(b.sessionHash, b.provider)
	b.reqLog.Debug("openai.provider_selected", zap.Int64("provider_id", b.provider.Record.ID), zap.String("provider_name", b.provider.Record.Name))
	gatewayhttp.SetOpsSelectedProvider(b.c, b.provider.Record.RoutingSnapshot())

	return b.selectedView(), nil
}

// SelectionFailure writes terminal Responses selection output unless the client
// is gone. An initial Compact capacity error receives its specific response;
// other initial errors use group-model diagnosis and scheduler refinement.
// Local capability failures retain their own status and reason. After provider
// exclusions, the last classified upstream failure takes
// precedence over a simple 502 fallback.
func (b *responsesAttemptBridge) SelectionFailure(err error, excludedCount int, last *textflow.AttemptFailure) {
	var lastFailoverErr *forwardcore.UpstreamFailoverError
	if last != nil {
		errors.As(last.Cause, &lastFailoverErr)
	}
	// A retry may exhaust candidate capacity before another HTTP exchange. The
	// last converted failure still owns the final status and Retry-After.
	if lastFailoverErr != nil && lastFailoverErr.Reason == forwardcore.OpenAIResponsesCompatibilityReason && !routing.IsModelAttributeError(err) {
		b.Exhausted(last)
		return
	}
	started, writable := b.responseErrorOutputMode()
	if !writable || gatewayhttp.WriteTextParameterCandidateError(b.c, protocol.ProtocolOpenAIResponses, err, started) || gatewayhttp.WriteOpenAIModelAttributeError(b.c, err, started) {
		return
	}
	b.reqLog.Warn("openai.provider_select_failed",
		zap.Error(gatewayhttp.OpenAICompatibleSelectionErrorForLog(err, b.requestPlatform)),
		zap.Int("excluded_provider_count", excludedCount),
	)
	if errors.Is(err, errResponsesSelectionEmpty) {
		cls := ClassifyOpenAICompatibleNoProviderErrorFromGin(b.c, b.binding().diagnoser, b.apiKey, b.reqModel, b.reqModel)
		if !cls.ModelNotFound {
			gatewayhttp.MarkOpsRoutingCapacityLimited(b.c)
		}
		b.binding().handleStreamingAwareError(b.c, cls.Status, cls.ErrType, cls.Message, started)
		return
	}
	if excludedCount == 0 {
		if b.legacyCompact && errors.Is(err, scheduler.ErrNoAvailableCompactProviders) {
			gatewayhttp.MarkOpsRoutingCapacityLimitedIfNoAvailable(b.c, err)
			b.binding().handleStreamingAwareError(b.c, http.StatusServiceUnavailable, "compact_not_supported", "No available providers support /responses/compact", started)
			return
		}
		cls := ClassifyNoProviderErrorFromGin(b.c, b.binding().diagnoser, b.apiKey, b.reqModel, b.reqModel, b.requestPlatform)
		cls = classifySelectionFailureError(err, cls)
		if !cls.ModelNotFound {
			gatewayhttp.MarkOpsRoutingCapacityLimitedIfNoAvailable(b.c, err)
		}
		b.binding().handleStreamingAwareError(b.c, cls.Status, cls.ErrType, cls.Message, started)
		return
	}
	b.Exhausted(last)
}

// Acquire tracks the selected session and adopts or waits for provider capacity.
// An eligible Responses candidate with a wait plan defers acquisition until
// Forward has prepared the exact tuple. A prefetched slot is adopted here so a
// preparation failure still leaves it with the usual forwarding cleanup.
func (b *responsesAttemptBridge) Acquire() bool {
	if b.sessionAttempts != nil && b.binding().sessions.Track != nil {
		b.binding().sessions.Track(b.sessionAttempts, b.provider, b.sessionHash)
	}
	if b.waitsAfterPreparation() {
		return true
	}
	return b.acquireSelectedSlot(b.selection)
}

// waitsAfterPreparation reports whether capacity has yet to be acquired for a rewrite candidate.
func (b *responsesAttemptBridge) waitsAfterPreparation() bool {
	return b.selection != nil && !b.selection.Acquired && b.selection.WaitPlan != nil &&
		gatewayhttp.ResponsesMayDrain(b.c, b.provider)
}

// acquireSelectedSlot takes ownership of the current selection's capacity.
// The existing slot helper writes a local rejection when it refuses admission.
// Forward owns the release on success.
func (b *responsesAttemptBridge) acquireSelectedSlot(selection *gatewaycapture.SelectionResult) bool {
	var acquired bool
	b.providerReleaseFunc, acquired = b.binding().acquireResponsesProviderSlot(b.c, b.apiKey.GroupID, b.sessionHash, selection, b.reqStream, b.streamStarted, b.reqLog)
	if !acquired {
		// Forward cannot run after failed admission. Retire any cleanup handed
		// back with the rejection here so it has the same single owner.
		b.releaseSelectedSlot()
	}
	return acquired
}

// releaseSelectedSlot returns the current lease before a local aggregation
// wait or after the exchange. Clearing it keeps later cleanup harmless.
func (b *responsesAttemptBridge) releaseSelectedSlot() {
	if b.providerReleaseFunc != nil {
		b.providerReleaseFunc()
		b.providerReleaseFunc = nil
	}
}

// responsesProviderCapacity preserves prefetch for an immediately admitted
// conversion and yields it on contention. Readmission keeps the same target,
// borrowing the configured alternate wait limits only when the prefetch had
// no plan. All callbacks run on the serial forwarding goroutine.
func (b *responsesAttemptBridge) responsesProviderCapacity() gatewayhttp.ResponsesProviderCapacity {
	slotReady := !b.waitsAfterPreparation()
	selected := b.selection
	return gatewayhttp.ResponsesProviderCapacity{
		Acquire: func(ctx context.Context) error {
			if slotReady {
				return nil
			}
			// Admission uses the same deadline for Redis, the wait counter and
			// polling. Restore the inbound context for output and completion.
			request := b.c.Request
			b.c.Request = request.WithContext(ctx)
			defer func() { b.c.Request = request }()
			slotReady = b.acquireSelectedSlot(selected)
			if err := context.Cause(ctx); err != nil {
				return err
			}
			if !slotReady {
				return textflow.ErrExecutionRejected
			}
			return nil
		},
		Yield: func() {
			if b.providerReleaseFunc == nil {
				return
			}
			// A spent prefetch cannot be adopted again. Preserve the immutable
			// selection used by route feedback and admit a fresh slot for its provider.
			b.releaseSelectedSlot()
			retry := *selected
			retry.Acquired, retry.ReleaseFunc = false, nil
			if retry.WaitPlan == nil {
				plan := b.binding().responsesReadmission
				plan.ProviderID, plan.MaxConcurrency = b.provider.Record.ID, b.provider.Record.Concurrency
				retry.WaitPlan = &plan
			}
			selected = &retry
			slotReady = false
		},
	}
}

// Forward executes canonical-derived Responses input and returns observed
// attempt state. Callers adopt prefetched capacity first and may defer a wait
// until native request preparation. Provider cleanup
// runs when forwarding returns, before Cyber policy recording and completion
// capture, while the bridge retains provider and result metadata for those
// later steps.
//
// Adjusted pre-forward output size excludes all recorded heartbeat bytes.
// Response timing subtracts a positive upstream phase only when total
// forwarding duration exceeds it; otherwise it retains the full duration.
// Result presence, images, native partial usage, HTTP commitment, and
// classified retry permission are recorded separately so the outer loop can
// decide replay or completion.
func (b *responsesAttemptBridge) Forward() textflow.ResponseOutcome {
	var err error
	gatewayhttp.SetOpsLatencyMs(b.c, gatewayhttp.OpsRoutingLatencyMsKey, time.Since(b.routingStart).Milliseconds())
	forwardStart := time.Now()
	// Recorded heartbeat bytes can commit headers before usable
	// output. Subtract those bytes here so later retry readiness compares
	// the actual exchange output.
	b.writerSizeBeforeForward = gatewayhttp.HeartbeatAdjustedWrittenSize(b.c)
	// A managed-mode provider after passthrough needs separately derived
	// reasoning input. Rebuild from canonical bytes so a previous
	// provider's sanitation does not replace future input.
	attemptBody := b.binding().deriveOpenAIForwardAttemptBody(b.reqLog, b.forwardBody, b.provider, &b.passthroughFailoverState)
	ctx := b.c.Request.Context()
	if gatewayhttp.ResponsesMayDrain(b.c, b.provider) {
		ctx = gatewayhttp.WithResponsesProviderCapacity(ctx, b.responsesProviderCapacity())
	}
	// Retire acquired provider capacity when the exchange returns on every
	// path. Keep its result and identity available for subsequent Cyber and
	// completion snapshot processing.
	b.result, err = func() (*forwardcore.OpenAIResult, error) {
		defer b.releaseSelectedSlot()
		return b.binding().forward(ctx, b.c, b.provider, attemptBody)
	}()
	if errors.Is(err, textflow.ErrExecutionRejected) {
		return textflow.ResponseOutcome{Outcome: textflow.Outcome{Stop: true, Err: err}}
	}
	converted := b.observeResponsesSettlement(err, forwardStart)
	var cyberBlockBodyHTTP []byte
	if gatewayhttp.GetOpsCyberPolicy(b.c) != nil {
		cyberBlockBodyHTTP = b.sessionHashBody
	}
	// For converted requests, moderation can still record policy decisions and
	// block the session. The final metering owner alone may submit funds.
	b.cyberPolicyHandled = b.binding().recordCyberPolicyIfMarked(b.c, b.apiKey, b.provider, b.subscription, b.reqModel, err != nil && !converted, cyberBlockBodyHTTP, gatewayhttp.ClientRequestedUsageFields(b.c, b.groupMapping, b.reqModel, ""), billing.HashUsageRequestPayload(b.body), b.nativeCompactionV2)
	forwardDurationMs := time.Since(forwardStart).Milliseconds()
	upstreamLatencyMs, _ := GetContextInt64(b.c, gatewayhttp.OpsUpstreamLatencyMsKey)
	responseLatencyMs := forwardDurationMs
	// Subtract only a positive upstream phase smaller than the measured
	// total. Missing or incompatible timing keeps total duration rather
	// than manufacturing negative response time.
	if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
		responseLatencyMs = forwardDurationMs - upstreamLatencyMs
	}
	gatewayhttp.SetOpsLatencyMs(b.c, gatewayhttp.OpsResponseLatencyMsKey, responseLatencyMs)

	out := textflow.ResponseOutcome{Outcome: textflow.Outcome{Attempt: openAIObservedAttempt(b.result, err), Err: err, HasResult: b.result != nil}, Images: b.result != nil && b.result.ImageCount > 0}
	out.NativePartial = err != nil && b.result != nil && b.result.NativeUsage != nil && (b.result.NativeUsage.HasObservedTokens() || b.result.ImageCount > 0)
	out.Attempt.HTTPCommitted = b.c.Writer.Written()
	if retry, ok := errors.AsType[*forwardcore.UpstreamFailoverError](err); ok {
		out.ReplayClosed = retry.Reason == forwardcore.OpenAIResponsesCompatibilityReason && retry.NextProviderAction == forwardcore.NextProviderStop
		out.Failure = &textflow.AttemptFailure{Cause: err, Policy: retry.RetryFailure()}
		out.FirstOutputRecovery = retry.SafeToFailoverAfterWrite
	}
	return out
}

// Complete captures Responses usage and request metadata before submitting its
// recording task. A nil result ends immediately. A converted result passes
// through the metering gate and freezes amounts before mandatory submission.
// Other Responses callers retain their existing asynchronous pricing policy.
func (b *responsesAttemptBridge) Complete() {
	res := b.result
	if res == nil || b.completionCalled {
		return
	}
	b.completionCalled = true
	if res.ResponsesSettlement != nil {
		b.completeConvertedResponses(res)
		return
	}
	completionInput := b.captureCompletion(gatewaycapture.OpenAICapture{
		Result:             res,
		RequestPayloadHash: billing.HashUsageRequestPayload(b.body),
		PricingUsageFields: b.groupMapping.ToUsageFields(b.reqModel, res.UpstreamModel),
		NativeCompactionV2: b.nativeCompactionV2,
	})
	completionUserID := b.subject.UserID
	completionModel := b.reqModel
	completionRuntime := b.binding().recorder
	b.binding().submitOpenAIUsageRecordTask(b.c, res, func(ctx context.Context) {
		if err := completionRuntime.Record(ctx, completionInput, true); err != nil {
			logging.L().With(
				zap.String("component", "handler.openai_gateway.responses"),
				zap.Int64("user_id", completionUserID),
				zap.Int64("api_key_id", completionInput.APIKey.ID),
				zap.Any("group_id", completionInput.APIKey.GroupID),
				zap.String("model", completionModel),
				zap.Int64("provider_id", completionInput.Provider.ID),
			).Error("openai.record_usage_failed", zap.Error(err))
		}
	})
}

// captureCompletion fills HTTP metadata and delegates snapshot creation to
// CaptureOpenAI. It stamps requested reasoning on the result and reads the
// current effective key, provider, subscription, body, quota updater, and Cyber
// handling state synchronously. Callers complete this capture before handing
// its result to asynchronous submission.
func (b *responsesAttemptBridge) captureCompletion(capture gatewaycapture.OpenAICapture) *completion.Input {
	gatewayhttp.StampOpenAIRequestedReasoningEffort(capture.Result, b.c)
	capture.UserAgent = b.c.GetHeader("User-Agent")
	capture.IPAddress = clientip.GetClientIP(b.c)
	capture.InboundEndpoint = gatewayhttp.GetInboundEndpoint(b.c)
	capture.UpstreamEndpoint = ResolveOpenAIUpstreamEndpoint(b.c, b.provider, capture.Result)
	capture.ClientSessionID = gatewayhttp.ExtractClientSessionID(b.c)
	capture.APIKey = b.apiKey
	capture.User = b.apiKey.User
	capture.Provider = gatewaycapture.ExecutionCompletionRecord(b.provider)
	capture.Subscription = b.subscription
	capture.RequestBody = b.body
	capture.APIKeyService = b.binding().apiKeyService
	capture.CyberBlocked = b.cyberPolicyHandled
	return gatewaycapture.CaptureOpenAI(gatewayhttp.CompletionContext(b.c), &capture)
}

// PartialImages logs a forwarding error alongside images observed by the
// current provider.
func (b *responsesAttemptBridge) PartialImages(err error) {
	b.reqLog.Warn("openai.forward_partial_error_with_image_result",
		zap.Int64("provider_id", b.provider.Record.ID),
		zap.Int("image_count", b.result.ImageCount),
		zap.Error(err),
	)
}

// RetryReady permits a classified Responses retry while the client and output
// allow replay. Callers supply a nonnil attempt failure. Nonfailover causes or
// client disconnection return false; Cyber warning handling is attempted before
// checking adjusted output through OpenAIForwardMayFailover.
//
// When replay is closed, health failure is observed before returning false.
// RunResponses owns terminal delivery separately. Allowed retries can retain
// committed headers; that sets the shared stream marker for exhaustion output.
// Eligible scheduling failures are reported only after this replay decision
// accepts another attempt.
func (b *responsesAttemptBridge) RetryReady(failure *textflow.AttemptFailure) bool {
	err := failure.Cause
	var failoverErr *forwardcore.UpstreamFailoverError
	if !errors.As(err, &failoverErr) {
		return false
	}
	if b.Context().Err() != nil {
		b.reqLog.Info("openai.failover_aborted_client_disconnected",
			zap.Int64("provider_id", b.provider.Record.ID),
			zap.Int("upstream_status", failoverErr.StatusCode),
		)
		return false
	}
	b.binding().recordOpenAICyberWarning(b.c, b.reqLog, b.apiKey, b.provider, b.reqModel, failoverErr.StatusCode, failoverErr.ResponseBody, err.Error())
	_, writable := b.responseErrorOutputMode()
	if !writable || !OpenAIForwardMayFailover(b.c, b.writerSizeBeforeForward, failoverErr) {
		b.binding().observeOpenAIProviderHealthFailure(b.c.Request.Context(), b.provider, err)
		return false
	}
	// Replay may remain safe after headers were committed. Mark the stream
	// before another attempt so exhausted recovery uses the protocol's
	// streaming terminal error path.
	if b.streamStarted != nil && gatewayhttp.CommittedSSE(b.c) {
		*b.streamStarted = true
	}
	if failoverErr.ShouldReportProviderScheduleFailure() {
		b.binding().reportOpenAIProviderScheduleResult(b.provider, OpenAIProviderScheduleModel(b.c, b.provider, b.forwardModel, b.requireCompact, nil), false, nil, err)
	}
	return true
}

// RetryWait logs the selected provider's same-provider retry allowance and
// delay. Callers supply a failure carrying UpstreamFailoverError. The outer
// text loop owns the wait and cancellation; this callback records its chosen
// parameters.
func (b *responsesAttemptBridge) RetryWait(failure *textflow.AttemptFailure, retryLimit, retryCount int, retryDelay time.Duration) {
	var failoverErr *forwardcore.UpstreamFailoverError
	errors.As(failure.Cause, &failoverErr)
	b.reqLog.Warn("openai.pool_mode_same_provider_retry",
		zap.Int64("provider_id", b.provider.Record.ID),
		zap.Int("upstream_status", failoverErr.StatusCode),
		zap.Int("retry_limit", retryLimit),
		zap.Int("retry_count", retryCount),
		zap.Duration("retry_delay", retryDelay),
	)
}

// Switching logs the selected provider and configured switch budget with proxy
// metadata. Callers supply a classified upstream failure. The outer text loop
// owns exclusion and switching; this callback reports the impending provider
// change.
func (b *responsesAttemptBridge) Switching(failure *textflow.AttemptFailure, switchCount, maxProviderSwitches int) {
	var failoverErr *forwardcore.UpstreamFailoverError
	errors.As(failure.Cause, &failoverErr)
	failoverSwitchFields := []zap.Field{
		zap.Int64("provider_id", b.provider.Record.ID),
		zap.Int("upstream_status", failoverErr.StatusCode),
		zap.Int("switch_count", switchCount),
		zap.Int("max_switches", maxProviderSwitches),
	}
	failoverSwitchFields = AppendOpenAIProviderProxyLogFields(failoverSwitchFields, b.provider)
	b.reqLog.Warn("openai.upstream_failover_switching", failoverSwitchFields...)
}

// OtherFailure presents local capability errors before recording any upstream
// failure feedback. For other errors it records Cyber warnings and scheduling
// failure, then supplies missing Responses error output. It prefers warning evidence carried by the
// error chain; absent evidence sends the error text to the warning recorder.
//
// After warning handling, communicated-error evidence and adjusted output size
// decide whether another terminal response is needed. A recorded warning
// permits fallback while adjusted size remains unchanged, and an already
// communicated error suppresses fallback. The selected output state is retained
// for Failed to choose log severity.
func (b *responsesAttemptBridge) OtherFailure(err error) {
	if requestpolicy.IsCandidateError(err) {
		if started, writable := b.responseErrorOutputMode(); writable {
			b.wroteFallback = gatewayhttp.WriteTextParameterCandidateError(b.c, protocol.ProtocolOpenAIResponses, err, started)
		}
		return
	}
	// Attribute admission can fail before transport. Present it once and stop
	// here so a local configuration rejection cannot reduce provider health scores.
	if routing.IsModelAttributeError(err) {
		if started, writable := b.responseErrorOutputMode(); writable {
			b.wroteFallback = b.binding().ensureOpenAIForwardErrorResponse(b.c, started, err)
		}
		return
	}
	statusCode := 0
	if v, ok := GetContextInt64(b.c, gatewayhttp.OpsUpstreamStatusCodeKey); ok {
		statusCode = int(v)
	}
	recordedWarning := b.binding().recordOpenAIForwardErrorCyberWarning(b.c, b.reqLog, b.apiKey, b.provider, b.reqModel, statusCode, err)
	if !recordedWarning {
		b.binding().recordOpenAICyberWarning(b.c, b.reqLog, b.apiKey, b.provider, b.reqModel, statusCode, nil, err.Error())
	}
	b.binding().reportOpenAIProviderScheduleResult(b.provider, OpenAIProviderScheduleModel(b.c, b.provider, b.forwardModel, b.requireCompact, b.result), false, nil, err)
	upstreamErrorAlreadyCommunicated := gatewayhttp.OpenAIForwardErrorAlreadyCommunicated(b.c, b.writerSizeBeforeForward, err)
	b.wroteFallback = false
	// Warning handling can emit terminal JSON or response.failed. Recheck
	// the communicated-error and adjusted-size evidence so supplying a
	// fallback does not duplicate that output.
	if !upstreamErrorAlreadyCommunicated && (!recordedWarning || gatewayhttp.HeartbeatAdjustedWrittenSize(b.c) == b.writerSizeBeforeForward) {
		if started, writable := b.responseErrorOutputMode(); writable {
			b.wroteFallback = b.binding().ensureOpenAIForwardErrorResponse(b.c, started, err)
		}
	}
	b.fields = []zap.Field{
		zap.Int64("provider_id", b.provider.Record.ID),
		zap.Bool("fallback_error_response_written", b.wroteFallback),
		zap.Bool("upstream_error_response_already_written", upstreamErrorAlreadyCommunicated),
		zap.Error(err),
	}
}

// Failed logs the stored Responses forwarding failure at its selected severity.
func (b *responsesAttemptBridge) Failed() {
	if gatewayhttp.ShouldLogOpenAIForwardFailureAsWarn(b.c, b.wroteFallback) {
		b.reqLog.Warn("openai.forward_failed", b.fields...)
		return
	}
	b.reqLog.Error("openai.forward_failed", b.fields...)
}

// Success reports the observed scheduling outcome and eligible OAuth quota
// headers. Nonshadow OpenAI OAuth results update the Codex snapshot; shadow
// providers skip that header path. The result's SucceededForScheduling decision
// and optional TTFT latency drive feedback independently of completion
// recording.
func (b *responsesAttemptBridge) Success() {
	if b.result != nil {
		// Shadow credentials obtain quota state through their separate
		// usage query; ordinary nonshadow OAuth results can update this
		// selected provider's snapshot from response headers.
		if b.provider.View().IsOpenAI() && b.provider.Record.Type == capability.ProviderTypeOAuth && !b.provider.View().IsShadow() {
			b.binding().updateCodexUsageSnapshotFromHeaders(b.c.Request.Context(), b.provider.Record.ID, b.result.ResponseHeaders)
		}
		b.binding().reportOpenAIProviderScheduleResult(b.provider, OpenAIProviderScheduleModel(b.c, b.provider, b.forwardModel, b.requireCompact, b.result), b.result.SucceededForScheduling(), b.result.Timings.TTFTMs())
	} else {
		b.binding().reportOpenAIProviderScheduleResult(b.provider, OpenAIProviderScheduleModel(b.c, b.provider, b.forwardModel, b.requireCompact, b.result), b.result.SucceededForScheduling(), nil)
	}
}

// Completed logs the serving Responses provider and final switch count.
func (b *responsesAttemptBridge) Completed(switchCount int) {
	b.reqLog.Debug("openai.request_completed",
		zap.Int64("provider_id", b.provider.Record.ID),
		zap.Int("switch_count", switchCount),
	)
}

// Context returns the current HTTP request context used by attempt
// coordination.
func (b *responsesAttemptBridge) Context() context.Context { return b.c.Request.Context() }

// CanAttempt reports whether the HTTP request remains available for provider
// replay. It delegates cancellation/disconnection checks to
// OpenAIRequestAllowsFailoverReplay. The separate RetryReady operation owns
// output-size permission after a forwarding failure.
func (b *responsesAttemptBridge) CanAttempt() bool { return OpenAIRequestAllowsFailoverReplay(b.c) }

// selectedView exposes the current provider's loop selection and OAuth retry
// classification. Callers retain a usable provider before invoking it. The view
// marks availability and distinguishes OpenAI OAuth-like from Grok OAuth
// credentials for retry policy.
func (b *responsesAttemptBridge) selectedView() textflow.ResponseSelection {
	return textflow.ResponseSelection{Selection: gatewaycapture.CaptureTextSelection(b.provider), Available: true, OAuth: failover.OAuth429Provider{OpenAI: b.provider.View().IsOpenAIOAuthLike(), Grok: b.provider.Record.Platform == capability.PlatformGrok && b.provider.Record.Type == capability.ProviderTypeOAuth}}
}

// Exhausted writes terminal Responses recovery output for the last attempt
// failure. A classified upstream cause uses its captured metadata. A converted
// failure retains its upstream Ops diagnosis even if the client cannot receive
// another response. Missing classification uses the simple 502 path.
func (b *responsesAttemptBridge) Exhausted(failure *textflow.AttemptFailure) {
	var original *forwardcore.UpstreamFailoverError
	if failure != nil {
		errors.As(failure.Cause, &original)
	}
	if original != nil && original.Reason == forwardcore.OpenAIResponsesCompatibilityReason {
		b.binding().handleFailoverExhausted(b.c, original, false)
		return
	}
	started, writable := b.responseErrorOutputMode()
	if !writable {
		return
	}
	if original != nil {
		b.binding().handleFailoverExhausted(b.c, original, started)
		return
	}
	b.binding().handleFailoverExhaustedSimple(b.c, http.StatusBadGateway, started)
}

// responseErrorOutputMode permits terminal output only on a live writable
// response. Native streams and Compact can append one SSE terminal after the
// writer has actually committed SSE. Synchronous requests and already
// terminated streams leave existing bytes intact, regardless of the slot's
// stream marker.
func (b *responsesAttemptBridge) responseErrorOutputMode() (started, writable bool) {
	return gatewayhttp.ResponsesErrorOutputMode(b.c, b.reqStream || b.requireCompact || b.nativeCompactionV2)
}

// Switched abandons the previous provider's session and records its selection
// switch. Session abandonment is conditional on tracking and provider presence.
// Scheduling feedback uses the retained selection after the serial loop decides
// to move providers.
func (b *responsesAttemptBridge) Switched() {
	if b.sessionAttempts != nil && b.provider != nil {
		b.sessionAttempts.Abandon(b.provider.Record.ID)
	}
	b.binding().recordOpenAIProviderSwitchForSelection(b.selection)
}
