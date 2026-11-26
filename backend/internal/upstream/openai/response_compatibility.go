package openai

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	wire "github.com/TokenFlux/TokenRouter/internal/protocol/openai"
	"github.com/TokenFlux/TokenRouter/internal/upstream"
)

// response_compatibility.go connects the two synchronous Responses adapters
// to the strict reader selected for an actual stream conversion. Upstream
// resolution and client publication have different commit points. A terminal
// frame can establish evidence while more bytes are still in flight, so only a
// finished resolution can authorize publication of a response or a replay
// decision.
//
// Each attempt exposes a snapshot to the executor even if it returns a read
// error and no client bytes. Resolved errors carry the upstream semantic
// status for the outer owner of error delivery. The two adapters retain their
// separate identity restoration and header-copy policies.

// These resolutions describe the upstream outcome independently of delivery
// and the caller's decisions about retry and settlement.
const (
	ResponsesStreamCompleted     ResponsesStreamResolution = "completed"
	ResponsesStreamIncomplete    ResponsesStreamResolution = "incomplete"
	ResponsesStreamRefused       ResponsesStreamResolution = "refused"
	ResponsesStreamFailed        ResponsesStreamResolution = "failed"
	ResponsesStreamProtocolError ResponsesStreamResolution = "protocol_error"

	// Evidence states distinguish unreported, fully reported zero, positive,
	// and unverified counters even when all their numeric fields are zero.
	ResponsesUsageMissing    ResponsesUsageState = "missing"
	ResponsesUsageZero       ResponsesUsageState = "zero"
	ResponsesUsageReported   ResponsesUsageState = "reported"
	ResponsesUsageUnverified ResponsesUsageState = "unverified"

	// Sources let settlement distinguish an independent observation from a
	// terminal report that can be invalidated by a later contradictory frame.
	ResponsesUsageIndependent ResponsesUsageSource = "independent"
	ResponsesUsageTerminal    ResponsesUsageSource = "terminal"
	ResponsesUsageCombined    ResponsesUsageSource = "combined"
)

// ResponsesStreamResolution records the terminal verdict of the strict reader.
type ResponsesStreamResolution string

// ResponsesUsageState records whether the observed counters establish usable metering.
type ResponsesUsageState string

// ResponsesUsageSource identifies the stream observations behind a metering snapshot.
type ResponsesUsageSource string

// ResponsesUsageEvidence carries the metering decision and supporting normalized counters.
type ResponsesUsageEvidence struct {
	State  ResponsesUsageState  // validity and presence decision
	Source ResponsesUsageSource // origin of the snapshot
	Usage  wire.ForwardUsage    // counters for reported or zero
}

// ResponsesStreamSnapshot describes one attempt without exposing mutable reader internals.
type ResponsesStreamSnapshot struct {
	Resolution    ResponsesStreamResolution // final verdict, empty while reading
	ResponseID    string                    // bound upstream Response ID
	ServiceTier   string                    // validated actual processing tier
	CleanEOF      bool                      // complete HTTP message and SSE framing
	FirstOutput   bool                      // observed content, execution, or terminal
	ReplayBlocked bool                      // monotonic during this attempt
	ReplayAllowed bool                      // proven pre-execution refusal after reading

	// Usage selects the counters usable at resolution. IndependentUsage is
	// maintained separately so a rejected terminal cannot overwrite its own
	// corroborating evidence.
	Usage            ResponsesUsageEvidence // final selected token evidence
	IndependentUsage ResponsesUsageEvidence // same-ID nonterminal token evidence
}

// ResponsesStreamAttempt carries reader hooks and observations for one exchange.
// Its zero value starts with no replay permission. Set hooks before reading;
// the read loop is the sole writer and other goroutines may take snapshots.
type ResponsesStreamAttempt struct {
	// OnRead observes positive physical reads, including comments and heartbeats.
	// OnFirstOutput observes the first semantic event that ends first-output
	// waiting. Both run synchronously on the reader before it reads again.
	OnRead        func() // optional physical read notification
	OnFirstOutput func() // optional first semantic notification

	// Transport holds the clocks for an attempt sent by Exchange. Standalone
	// reader callers do not need to install transport policy.
	Transport ResponsesTransportOptions
	transport *responsesTransport // exchange state owned by the forwarding goroutine

	mu       sync.Mutex              // serializes publication and snapshots
	snapshot ResponsesStreamSnapshot // latest attempt observation
}

// ResponsesStreamError carries a classified failure before downstream error delivery.
type ResponsesStreamError struct {
	StatusCode int    // downstream semantic status
	EventType  string // classified upstream terminal event
	Payload    []byte // upstream error frame for policy
	Message    string // failure description for translation
	// DirectHTTPRejection marks a complete standalone HTTP error envelope,
	// which can authorize an explicitly identified request-field repair.
	DirectHTTPRejection bool
	cause               error // retained read or validation cause
}

// Snapshot returns a detached copy for timing, replay and completion decisions.
// ReplayAllowed is usable only after the read loop has returned and the executor
// has checked its own cancellation and downstream-commit state.
func (a *ResponsesStreamAttempt) Snapshot() ResponsesStreamSnapshot {
	if a == nil {
		return ResponsesStreamSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	snapshot := a.snapshot
	for _, evidence := range []*ResponsesUsageEvidence{&snapshot.Usage, &snapshot.IndependentUsage} {
		if details := evidence.Usage.CacheWriteDetails; details != nil {
			evidence.Usage.CacheWriteDetails = new(*details)
		}
	}
	return snapshot
}

// BlockReplay closes the finished attempt after transport or gateway recovery
// checks. Callers use it after the reader returns or when no reader was started.
func (a *ResponsesStreamAttempt) BlockReplay() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.snapshot.ReplayBlocked = true
	a.snapshot.ReplayAllowed = false
}

// publish updates the snapshot before notifying the first-output timing hook.
// The hook is invoked outside the mutex, so it may safely read the snapshot.
func (a *ResponsesStreamAttempt) publish(snapshot ResponsesStreamSnapshot) {
	a.mu.Lock()
	first := snapshot.FirstOutput && !a.snapshot.FirstOutput
	a.snapshot = snapshot
	a.mu.Unlock()
	if first && a.OnFirstOutput != nil {
		a.OnFirstOutput()
	}
}

// Error returns the semantic failure description for diagnostics and translation.
func (e *ResponsesStreamError) Error() string { return e.Message }

// Unwrap exposes the original read or validation error to the caller.
func (e *ResponsesStreamError) Unwrap() error { return e.cause }

// readResponsesCompatibility resolves upstream input and publishes one synchronous
// JSON response after validation and client identity restoration. The caller
// owns Body closure and the error writer, and receives partial observations
// alongside a classified error even when client output is suppressed.
func readResponsesCompatibility(ctx context.Context, resp *http.Response, out *upstream.OutputContext, options NonStreamOptions, originalModel string, restore func([]byte) ([]byte, error)) (*NonStreamingResult, error) {
	resolver := newResponsesStreamResolver(options.Compatibility)
	resolver.countSearch = options.CountJSONSearch
	cleanEOF, readErr := readResponsesCompatibilityBody(resp.Body, resolver, resp.StatusCode)
	var rejection *ResponsesStreamError
	if errors.As(readErr, &rejection) && rejection.DirectHTTPRejection {
		replay, started := responsesExecutionProof(rejection.Payload)
		execution := responsesErrorObject(rejection.Payload).Get("execution_started")
		fieldRejection := rejection.StatusCode == http.StatusBadRequest && (!execution.Exists() || execution.Type == gjson.False)
		options.Compatibility.publish(ResponsesStreamSnapshot{
			Resolution: ResponsesStreamFailed, CleanEOF: cleanEOF,
			FirstOutput: true, ReplayAllowed: replay,
			ReplayBlocked:    started || (!replay && !fieldRejection),
			Usage:            ResponsesUsageEvidence{State: ResponsesUsageMissing},
			IndependentUsage: ResponsesUsageEvidence{State: ResponsesUsageMissing},
		})
		return nil, rejection
	}
	body, result, err := resolver.resolve(cleanEOF, readErr, resp.StatusCode)
	if err != nil {
		return result, err
	}
	if options.ObserveTier != nil {
		options.ObserveTier(body)
	}
	if originalModel != "" {
		body, err = sjson.SetBytes(body, "model", originalModel)
		if err != nil {
			return result, err
		}
	}
	if options.CorrectToolCalls != nil {
		body = options.CorrectToolCalls(body)
	}
	body, err = restore(body)
	if err != nil {
		return result, err
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return result, responsesReadError(errors.New("restored upstream Responses result is not a JSON object"))
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := options.Compatibility.TransportFailure(); err != nil {
		return result, err
	}
	if out.Writer.Written() {
		return result, errors.New("synchronous Responses output was already committed")
	}
	// Set the representation after header filtering, which may copy the
	// upstream event-stream media type or its byte count into the sink.
	options.ResponseHeaders(out.Writer.Header(), resp.Header)
	options.JSONOnly = true
	options.prepareJSONHeaders(out.Writer.Header())
	out.Data(http.StatusOK, "application/json", body)
	err = out.Err()
	if options.CompatibilityDelivered != nil {
		options.CompatibilityDelivered(options.Compatibility.Snapshot().Resolution, err)
	}
	return result, err
}

// responsesReadError preserves deadline and cancellation causes for timeout
// policy while translating an interrupted or malformed upstream stream to 502.
func responsesReadError(err error) *ResponsesStreamError {
	if classified, ok := errors.AsType[*ResponsesStreamError](err); ok {
		return classified
	}
	result := &ResponsesStreamError{StatusCode: http.StatusBadGateway, Message: err.Error(), cause: err}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		result.StatusCode = http.StatusGatewayTimeout
	}
	return result
}
