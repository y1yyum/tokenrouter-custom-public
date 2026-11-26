package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/requestcontext"
)

// response_compatibility_transport.go bounds the HTTP exchange for a converted
// Responses attempt. The first semantic output budget spans response headers
// and body reads. Physical bytes move only the idle deadline, so upstream
// heartbeats cannot satisfy the first-output budget or extend the request.
//
// The request context retains server abort and request values while allowing a
// disconnected client's existing response body to yield trailing usage. A
// disconnect during header wait cancels immediately. Once a body is available,
// cancellation interrupts the network read. The forwarding goroutine closes
// the body after that read returns, including when a transport decoder owns
// state that cannot be accessed by Read and Close concurrently.

const (
	// responsesMaxDrain is the longest body drain after the client leaves.
	responsesMaxDrain = 5 * time.Second
)

// These causes identify which clock ended a converted request for HTTP and Ops.
var (
	ErrResponsesHeaderTimeout      = errors.New("upstream Responses timed out waiting for response headers")
	ErrResponsesFirstOutputTimeout = errors.New("upstream Responses timed out waiting for first output")
	ErrResponsesReadIdleTimeout    = errors.New("upstream Responses timed out between upstream reads")
	ErrResponsesDeadline           = errors.New("upstream Responses request deadline exceeded")
)

// ResponsesTransportOptions supplies monotonic clock origins and positive read budgets.
type ResponsesTransportOptions struct {
	StartedAt          time.Time     // selected attempt start
	Deadline           time.Time     // shared logical request cutoff
	FirstOutputTimeout time.Duration // header and semantic output budget
	StreamInterval     time.Duration // maximum interval between physical reads

	// DrainTimeout optionally shortens the five-second client disconnect window,
	// for example in a test. Zero uses the production maximum.
	DrainTimeout time.Duration

	// OnDispatch optionally records handoff after the final cancellation checks.
	// Exchange invokes it synchronously once, immediately before calling send.
	OnDispatch func()
	// ReleaseConversion returns local aggregation capacity after the response
	// reader exits and the body closes, or immediately after an unsuccessful send.
	ReleaseConversion func()
}

// responsesTransport owns the cancellable exchange and its phase clocks.
type responsesTransport struct {
	ctx    context.Context         // cancellable HTTP exchange context
	cancel context.CancelCauseFunc // transport interruption with reason

	// The mutex protects deadline transitions across transport, reader and timer
	// callbacks. Only the forwarding goroutine installs the response body and
	// binds the semantic callback.
	mu           sync.Mutex         // serializes phase changes
	timer        *time.Timer        // wakes at the nearest cutoff
	scheduled    time.Time          // cutoff currently scheduled
	first        time.Time          // absolute first-output cutoff
	idle         time.Time          // sliding body read cutoff
	total        time.Time          // absolute logical request cutoff
	drain        time.Time          // absolute disconnect cutoff
	interval     time.Duration      // idle budget on each positive read
	drainTimeout time.Duration      // bounded disconnected read window
	stopClient   func() bool        // unregisters the disconnect observer
	stopDeadline context.CancelFunc // releases the request deadline context
	hasBody      bool               // response headers supplied a body
	finished     bool               // cleanup has stopped phase transitions
}

// responsesTransportBody connects HTTP reads and body closure to attempt timing.
type responsesTransportBody struct {
	body              io.ReadCloser       // original cancellable transport body
	state             *responsesTransport // phase and cancellation owner
	releaseConversion func()              // optional instance slot owned until Close

	// Only the forwarding owner closes the body, after its reader exits. Repeated
	// cleanup paths receive the same close result without repeating decoder cleanup.
	closeOnce sync.Once // serializes underlying close
	closeErr  error     // result of underlying close
}

// Exchange sends one converted request and returns its timed response body.
// The caller still owns reading and closing that body. client supplies the
// downstream cancellation signal and the request retains outbound values
// such as the redirect policy. Set Transport and reader hooks before calling.
//
// A downstream cancellation observed before handoff returns without invoking
// send. After handoff the response body can drain only within all remaining
// budgets. Transport errors close any returned body before the attempt exits.
func (a *ResponsesStreamAttempt) Exchange(client context.Context, request *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	release := a.Transport.ReleaseConversion
	defer func() {
		// A successful response takes ownership below. Failed sends release here
		// after closing any unclaimed upstream body and stopping exchange clocks.
		if release != nil {
			release()
		}
	}()
	if err := client.Err(); err != nil {
		closeResponsesRequestBody(request)
		return nil, err
	}
	state := newResponsesTransport(client, request.Context(), a.Transport)
	request = request.WithContext(state.ctx)
	if err := client.Err(); err != nil {
		state.finish()
		closeResponsesRequestBody(request)
		return nil, err
	}
	if err := state.failure(); err != nil {
		state.finish()
		closeResponsesRequestBody(request)
		return nil, err
	}

	// A provider can add an idempotency header. Prevent internal HTTP retries
	// from reopening a converted body before the gateway has execution evidence.
	request.GetBody = nil
	if dispatch := a.Transport.OnDispatch; dispatch != nil {
		dispatch()
	}
	response, err := send(request)
	if err == nil && response != nil && response.Body != nil {
		state.receivedHeaders(client.Err())
		if err = state.failure(); err == nil {
			previous := a.OnFirstOutput
			a.OnFirstOutput = func() {
				state.firstOutput()
				if previous != nil {
					previous()
				}
			}
			response.Body = &responsesTransportBody{body: response.Body, state: state, releaseConversion: release}
			release = nil
			a.transport = state
			return response, nil
		}
	}
	if cause := state.failure(); cause != nil {
		err = cause
	}
	state.finish()
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}

// closeResponsesRequestBody discards a body that never reached transport.
func closeResponsesRequestBody(request *http.Request) {
	if request.Body != nil {
		_ = request.Body.Close()
	}
}

// TransportFailure reports cancellation or the winning clock after handoff.
// Readers use it before publishing a completed JSON response. Call it on the
// forwarding goroutine after Exchange has installed the response body.
func (a *ResponsesStreamAttempt) TransportFailure() error {
	if a == nil || a.transport == nil {
		return nil
	}
	return a.transport.failure()
}

// newResponsesTransport reinstates the earlier of the caller and server
// deadlines on a detached context. It starts the first-output clock at the
// supplied attempt start, so preparation time consumes that same budget.
func newResponsesTransport(client, outbound context.Context, options ResponsesTransportOptions) *responsesTransport {
	total := options.Deadline
	if deadline, ok := client.Deadline(); ok && (total.IsZero() || deadline.Before(total)) {
		total = deadline
	}
	drainTimeout := options.DrainTimeout
	if drainTimeout <= 0 || drainTimeout > responsesMaxDrain {
		drainTimeout = responsesMaxDrain
	}
	deadlineCtx, stopDeadline := context.WithDeadlineCause(requestcontext.Detach(outbound), total, ErrResponsesDeadline)
	ctx, cancel := context.WithCancelCause(deadlineCtx)
	state := &responsesTransport{
		ctx: ctx, cancel: cancel, total: total, interval: options.StreamInterval, drainTimeout: drainTimeout,
		first:        options.StartedAt.Add(options.FirstOutputTimeout),
		stopDeadline: stopDeadline,
	}
	state.mu.Lock()
	state.schedule(time.Now())
	state.mu.Unlock()
	state.stopClient = context.AfterFunc(client, state.clientDisconnected)
	return state
}

// schedule updates the timer under mu or immediately cancels an expired
// exchange. Each timer callback recomputes the winning absolute cutoff under
// the same mutex. A callback awakened by an older deadline therefore cannot
// interrupt an attempt whose relevant phase already advanced. Callers check
// expiry before accepting new bytes or changing phases, so delayed callbacks
// also cannot let late progress move an already expired deadline.
func (s *responsesTransport) schedule(now time.Time) {
	if s.finished || s.ctx.Err() != nil {
		return
	}
	firstCause := ErrResponsesHeaderTimeout
	if s.hasBody {
		firstCause = ErrResponsesFirstOutputTimeout
	}
	cutoff, cause := s.total, ErrResponsesDeadline
	for _, candidate := range [...]struct {
		at    time.Time
		cause error
	}{{s.first, firstCause}, {s.idle, ErrResponsesReadIdleTimeout}, {s.drain, context.Canceled}} {
		if !candidate.at.IsZero() && (cutoff.IsZero() || candidate.at.Before(cutoff)) {
			cutoff, cause = candidate.at, candidate.cause
		}
	}
	remaining := cutoff.Sub(now)
	if remaining <= 0 {
		s.cancel(cause)
		return
	}
	if cutoff.Equal(s.scheduled) {
		return
	}
	s.scheduled = cutoff
	if s.timer == nil {
		s.timer = time.AfterFunc(remaining, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.scheduled = time.Time{}
			s.schedule(time.Now())
		})
		return
	}
	s.timer.Reset(remaining)
}

// receivedHeaders starts the read-interval clock without changing first-output
// time. If downstream cancellation was already visible during header wait, the
// exchange ends before the response is turned over for body reading.
func (s *responsesTransport) receivedHeaders(clientErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.schedule(now)
	if clientErr != nil {
		s.cancel(clientErr)
		return
	}
	s.hasBody = true
	s.idle = now.Add(s.interval)
	s.schedule(now)
}

// firstOutput retires the first-output clock when semantic progress arrives.
// The read-interval and total clocks continue to constrain the remaining body.
func (s *responsesTransport) firstOutput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.schedule(now)
	s.first = time.Time{}
	s.schedule(now)
}

// clientDisconnected stops header waits or starts a bounded drain of an
// existing response. Expired request or read deadlines keep their own cause,
// and the drain cutoff can only make the remaining lifetime shorter.
func (s *responsesTransport) clientDisconnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.schedule(now)
	if !s.hasBody {
		s.cancel(context.Canceled)
		return
	}
	s.drain = now.Add(s.drainTimeout)
	s.schedule(now)
}

// failure reports the first cancellation, translating clocks into a typed
// 504 before the HTTP layers classify failure or forward its diagnostic cause.
func (s *responsesTransport) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedule(time.Now())
	cause := context.Cause(s.ctx)
	switch cause {
	case ErrResponsesHeaderTimeout, ErrResponsesFirstOutputTimeout, ErrResponsesReadIdleTimeout, ErrResponsesDeadline:
		return &ResponsesStreamError{StatusCode: http.StatusGatewayTimeout, Message: cause.Error(), cause: cause}
	default:
		return cause
	}
}

// finish stops both observers and cancels any remaining exchange work.
// The forwarding goroutine uses it for failed sends and explicit body closure.
func (s *responsesTransport) finish() {
	s.mu.Lock()
	s.finished = true
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	s.stopClient()
	s.cancel(context.Canceled)
	s.stopDeadline()
}

// Read updates the idle clock after each physical read. EOF retires the idle
// clock while the buffered reader finishes semantic processing, which remains
// subject to the first-output and total deadlines until body closure.
func (r *responsesTransportBody) Read(p []byte) (int, error) {
	if err := r.state.failure(); err != nil {
		return 0, err
	}
	n, err := r.body.Read(p)
	s := r.state
	s.mu.Lock()
	now := time.Now()
	s.schedule(now)
	switch {
	case err != nil:
		s.idle = time.Time{}
	case n > 0:
		s.idle = now.Add(s.interval)
	}
	s.schedule(now)
	s.mu.Unlock()
	if cause := s.failure(); cause != nil {
		return n, cause
	}
	return n, err
}

// Close retires the exchange callbacks and closes the underlying body once.
// Callers invoke it after the reader has exited, before returning the slot.
func (r *responsesTransportBody) Close() error {
	r.closeOnce.Do(func() {
		r.state.finish()
		r.closeErr = r.body.Close()
		if r.releaseConversion != nil {
			r.releaseConversion()
		}
	})
	return r.closeErr
}
