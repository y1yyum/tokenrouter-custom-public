package openai

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/pkg/requestcontext"
)

// responsesClockBody models one response tail and an interruptible network read.
type responsesClockBody struct {
	ctx    context.Context // upstream transport context
	tail   <-chan string   // queued tail bytes
	closed int             // explicit body close count
}

// responsesClockValue identifies an outbound-only request value in the fixture.
type responsesClockValue struct{}

// TestResponsesTransportBeforeHandoff cancels the downstream request or
// exhausts the attempt before calling Exchange. Neither path invokes the
// transport, even though the request has already been built.
func TestResponsesTransportBeforeHandoff(t *testing.T) {
	for _, cancelled := range []bool{true, false} {
		client, cancel := context.WithCancel(t.Context())
		if cancelled {
			cancel()
		}
		now := time.Now()
		dispatches := 0
		releases := 0
		body := &responsesClockBody{}
		attempt := &ResponsesStreamAttempt{Transport: ResponsesTransportOptions{
			StartedAt: now.Add(-time.Second), Deadline: now.Add(time.Second),
			FirstOutputTimeout: time.Millisecond, StreamInterval: time.Second,
			OnDispatch: func() { dispatches++ },
			ReleaseConversion: func() {
				require.Equal(t, 1, body.closed)
				releases++
			},
		}}
		request, err := http.NewRequestWithContext(client, http.MethodPost, "http://upstream.invalid/v1/responses", body)
		require.NoError(t, err)
		_, err = attempt.Exchange(client, request, func(*http.Request) (*http.Response, error) {
			t.Error("transport called after a pre-send stop")
			return nil, io.ErrUnexpectedEOF
		})
		cancel()
		if cancelled {
			require.ErrorIs(t, err, context.Canceled)
		} else {
			require.ErrorIs(t, err, ErrResponsesHeaderTimeout)
		}
		require.Equal(t, 1, body.closed)
		require.Zero(t, dispatches)
		require.Equal(t, 1, releases)
	}
}

// TestResponsesTransportDrainLimits drives a response body after disconnect.
// The reader can consume a tail before the drain limit. Caller deadline and
// server abort separately stop the read before that window expires.
func TestResponsesTransportDrainLimits(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		end   time.Duration
		abort bool
		cause error
	}{
		{name: "drain_window", end: 5 * time.Second, cause: context.Canceled},
		{name: "earlier_caller_deadline", end: 2 * time.Second, cause: ErrResponsesDeadline},
		{name: "immediate_server_abort", end: time.Second, abort: true, cause: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				callerDeadline := started.Add(8 * time.Second)
				if scenario.cause == ErrResponsesDeadline {
					callerDeadline = started.Add(scenario.end)
				}
				parent, stop := context.WithDeadline(t.Context(), callerDeadline)
				defer stop()
				client, disconnect := context.WithCancel(parent)
				defer disconnect()
				client, abort := requestcontext.WithAbort(client)
				defer abort()
				deadline := started.Add(9 * time.Second)
				dispatches := 0
				releases := 0
				tail := make(chan string, 1)
				body := &responsesClockBody{tail: tail}
				attempt := &ResponsesStreamAttempt{Transport: ResponsesTransportOptions{
					StartedAt: started, Deadline: deadline, FirstOutputTimeout: time.Second, StreamInterval: 6 * time.Second,
					OnDispatch: func() { dispatches++ },
					ReleaseConversion: func() {
						require.Equal(t, 1, body.closed)
						releases++
					},
				}}
				request, err := http.NewRequestWithContext(context.WithValue(client, responsesClockValue{}, "retained"), http.MethodPost, "http://upstream.invalid/v1/responses", strings.NewReader("{}"))
				require.NoError(t, err)
				resp, err := attempt.Exchange(client, request, func(req *http.Request) (*http.Response, error) {
					require.Equal(t, 1, dispatches)
					body.ctx = req.Context()
					require.Equal(t, "retained", body.ctx.Value(responsesClockValue{}))
					actualDeadline, ok := body.ctx.Deadline()
					require.True(t, ok)
					require.Equal(t, callerDeadline, actualDeadline)
					return &http.Response{Body: body}, nil
				})
				require.NoError(t, err)
				require.Equal(t, 1, dispatches)
				attempt.OnFirstOutput()
				disconnect()
				synctest.Wait()
				require.NoError(t, body.ctx.Err())
				tail <- "usage"
				p := make([]byte, 8)
				n, err := resp.Body.Read(p)
				require.NoError(t, err)
				require.Equal(t, "usage", string(p[:n]))
				if scenario.abort {
					time.AfterFunc(scenario.end, abort)
				}
				_, err = resp.Body.Read(p)
				require.ErrorIs(t, err, scenario.cause)
				require.Equal(t, scenario.end, time.Since(started))
				require.Zero(t, releases, "capacity stays owned while the closed reader's body is still open")
				require.NoError(t, resp.Body.Close())
				require.NoError(t, resp.Body.Close())
				require.Equal(t, 1, body.closed)
				require.Equal(t, 1, releases)
			})
		})
	}
}

// TestResponsesTransportSendFailureReleasesConversion verifies that a failed
// transport relinquishes the instance slot after closing any unclaimed body.
func TestResponsesTransportSendFailureReleasesConversion(t *testing.T) {
	ctx := t.Context()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.invalid/v1/responses", strings.NewReader("{}"))
	require.NoError(t, err)
	body := &responsesClockBody{}
	releases := 0
	start := time.Now()
	attempt := &ResponsesStreamAttempt{Transport: ResponsesTransportOptions{
		StartedAt: start, Deadline: start.Add(time.Second), FirstOutputTimeout: time.Second,
		ReleaseConversion: func() {
			require.Equal(t, 1, body.closed)
			releases++
		},
	}}
	_, err = attempt.Exchange(ctx, request, func(req *http.Request) (*http.Response, error) {
		require.Nil(t, req.GetBody)
		return &http.Response{Body: body}, io.ErrUnexpectedEOF
	})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Equal(t, 1, releases)
}

// Read returns queued bytes or the cancellation reported by the transport.
func (b *responsesClockBody) Read(p []byte) (int, error) {
	select {
	case tail := <-b.tail:
		return copy(p, tail), nil
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

// Close records explicit body cleanup after the caller exits reading.
func (b *responsesClockBody) Close() error {
	b.closed++
	return nil
}
