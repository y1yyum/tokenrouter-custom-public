package openaiforward

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/gateway/compact"
	forwardcore "github.com/TokenFlux/TokenRouter/internal/gateway/forward"
	"github.com/TokenFlux/TokenRouter/internal/upstream"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// trackedBody counts network-body closes performed by the HTTP recovery runner.
type trackedBody struct {
	io.Reader     // supplied failure payload
	closes    int // recorded close attempts
}

// replayBodyProbe records a close and can signal client disconnection at that boundary.
type replayBodyProbe struct {
	trackedBody        // reader and close count
	onClose     func() // optional action during close
	closeErr    error  // optional simulated cleanup failure
}

// replayOutputProbe exposes the downstream writer state without emitting output.
type replayOutputProbe struct {
	cacheOutputSink      // unused output receiver
	committed       bool // previously committed response
}

// Close records one cleanup attempt and succeeds for this body fixture.
func (b *trackedBody) Close() error { b.closes++; return nil }

// Close records cleanup before the test publishes an optional cancellation or error.
func (b *replayBodyProbe) Close() error {
	_ = b.trackedBody.Close()
	if b.onClose != nil {
		b.onClose()
	}
	return b.closeErr
}

// OutputState reports whether client output was committed before recovery.
func (s *replayOutputProbe) OutputState() upstream.OutputHead {
	return upstream.OutputHead{Committed: s.committed}
}

// TestRunHTTPConvertedReplayChecksCleanupAndConnection supplies a complete
// pre-execution rejection and varies what the gateway observes at cleanup.
// It protects the final connection and writer checks even with positive reader proof.
func TestRunHTTPConvertedReplayChecksCleanupAndConnection(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		cancelOnClose bool
		committed     bool
		closeFails    bool
		replay        bool
	}{
		{name: "ready_after_close", replay: true},
		{name: "client_canceled_at_close", cancelOnClose: true},
		{name: "writer_committed", committed: true},
		{name: "cleanup_failed", closeFails: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			body := &replayBodyProbe{trackedBody: trackedBody{Reader: strings.NewReader(
				`{"error":{"code":"server_is_overloaded","message":"Overloaded","execution_started":false}}`,
			)}}
			if scenario.cancelOnClose {
				body.onClose = cancel
			}
			if scenario.closeFails {
				body.closeErr = errors.New("response body cleanup failed")
			}
			now := time.Now()
			attempt := &openai.ResponsesStreamAttempt{Transport: openai.ResponsesTransportOptions{
				StartedAt: now, Deadline: now.Add(5 * time.Second), FirstOutputTimeout: time.Second, StreamInterval: time.Second,
			}}
			result, err := RunHTTP(ctx, HTTPInput{
				Body:        []byte(`{"model":"gpt-6-luna"}`),
				PrepareSend: func(*http.Request) (*openai.ResponsesStreamAttempt, error) { return attempt, nil },
			}, HTTPOptions{
				Sink:             &replayOutputProbe{committed: scenario.committed},
				NonStreamOptions: func() openai.NonStreamOptions { return openai.NonStreamOptions{} },
				Exchange: openai.HTTPExchangeOptions{
					RequestContext: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} },
					Build: func(ctx context.Context, body []byte) (*http.Request, error) {
						return http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/v1/responses", strings.NewReader(string(body)))
					},
					ApplyHeaders: func(header http.Header) { header.Set("Idempotency-Key", "fixture") },
					Do: func(req *http.Request) (*http.Response, error) {
						require.Nil(t, req.GetBody, "the HTTP transport cannot reopen the converted POST")
						require.Equal(t, "fixture", req.Header.Get("Idempotency-Key"))
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: body}, nil
					},
					Latency: func(time.Duration) {},
				},
			})
			require.Nil(t, result)
			require.Equal(t, 1, body.closes)
			var failure *forwardcore.UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, http.StatusServiceUnavailable, failure.StatusCode)
			require.Equal(t, scenario.replay, failure.ShouldRetryNextProvider())
			require.Equal(t, scenario.replay, attempt.Snapshot().ReplayAllowed)
			require.Equal(t, !scenario.replay, attempt.Snapshot().ReplayBlocked)
		})
	}
}

// TestRunHTTPRecoveryBoundaries returns repeated HTTP rejection fixtures for
// agent, ciphertext, and ordinary failures. It protects one recovery per
// applicable budget, ciphertext lineage marking, terminal errors, and closure
// of each network body before retry.
func TestRunHTTPRecoveryBoundaries(t *testing.T) {
	for _, kind := range []string{"agent", "encrypted", "plain"} {
		t.Run(kind, func(t *testing.T) {
			bodies := []*trackedBody{}
			requests, recoveries, lineages := 0, 0, 0
			terminal := errors.New("original upstream failure")
			options := HTTPOptions{
				Exchange: openai.HTTPExchangeOptions{
					RequestContext: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} },
					Build: func(ctx context.Context, body []byte) (*http.Request, error) {
						return http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/responses", strings.NewReader(string(body)))
					},
					ApplyHeaders: func(http.Header) {}, Latency: func(time.Duration) {}, TransportError: func(err error) error { return err },
					Do: func(*http.Request) (*http.Response, error) {
						requests++
						b := &trackedBody{Reader: strings.NewReader(`{"error":{"message":"rejected"}}`)}
						bodies = append(bodies, b)
						return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: b}, nil
					},
				},
				// Fixture bodies wrap strings.Reader, whose reads have no transport
				// failures; recovery assertions therefore inspect bytes and close
				// counts.
				ReadErrorBody:    func(resp *http.Response) []byte { b, _ := io.ReadAll(resp.Body); return b },
				IsAgentIdentity:  func(context.Context) bool { return kind == "agent" },
				InvalidAgentTask: func(int, []byte) bool { return true },
				RecoverAgentTask: func(context.Context) error { recoveries++; return nil },
				RedactErrorBody:  func(_ context.Context, b []byte) []byte { return b },
				ErrorDetails: func([]byte) (string, string) {
					if kind == "encrypted" {
						return "rejected", "invalid_encrypted_content"
					}
					return "rejected", ""
				},
				RetryEncrypted: func([]byte) ([]byte, bool, error) {
					recoveries++
					return []byte(`{"model":"gpt-5","input":[]}`), true, nil
				},
				MarkInvalidLineage: func([]byte) { lineages++ },
				CompactRetry:       func([]byte, int, string, []byte, bool) ([]byte, string, bool) { return nil, "", false },
				ShouldFailover:     func(int, string, []byte) bool { return false },
				ErrorResponse:      func(*http.Response, []byte, string) error { return terminal },
				Log:                func(string, ...any) {},
			}
			result, err := RunHTTP(context.Background(), HTTPInput{Body: []byte(`{"model":"gpt-5"}`)}, options)
			require.Nil(t, result)
			require.ErrorIs(t, err, terminal)
			expected := 2
			if kind == "plain" {
				expected = 1
			}
			require.Equal(t, expected, requests)
			if kind == "plain" {
				require.Zero(t, recoveries)
			} else {
				require.Equal(t, 1, recoveries)
			}
			if kind == "encrypted" {
				require.Equal(t, 1, lineages)
			} else {
				require.Zero(t, lineages)
			}
			for _, b := range bodies {
				require.Equal(t, 1, b.closes, "实际网络响应体在恢复前关闭")
			}
		})
	}
}

// TestRunHTTPTransportFailureDoesNotRecover makes request construction fail
// before a response exists. The runner must return that same failure and a nil
// result before HTTP recovery callbacks are needed.
func TestRunHTTPTransportFailureDoesNotRecover(t *testing.T) {
	failure := errors.New("transport canceled")
	options := HTTPOptions{Exchange: openai.HTTPExchangeOptions{
		RequestContext: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} },
		Build:          func(context.Context, []byte) (*http.Request, error) { return nil, failure },
	}}
	result, err := RunHTTP(context.Background(), HTTPInput{}, options)
	require.Nil(t, result)
	require.ErrorIs(t, err, failure)
}

// TestRunHTTPNonStreamCompactRecoveryAndTerminalClassification verifies that
// buffered Compact reader failures close the failed attempt body before the
// next exchange, record CompactRetryObserved on retry, and route unrecovered
// Compact signals through CompactErrorResponse, ShouldFailover, and ErrorResponse.
func TestRunHTTPNonStreamCompactRecoveryAndTerminalClassification(t *testing.T) {
	for _, failoverEligible := range []bool{true, false} {
		name := "terminal_presenter"
		if failoverEligible {
			name = "compact_failover"
		}
		t.Run(name, func(t *testing.T) {
			var bodies []*trackedBody
			observedRetries := 0
			var observedModels []string
			failoverErr := errors.New("compact failover classified")
			presenterErr := errors.New("compact terminal presented")
			var presenterModel string

			options := HTTPOptions{
				Exchange: openai.HTTPExchangeOptions{
					RequestContext: func(ctx context.Context) (context.Context, context.CancelFunc) { return ctx, func() {} },
					Build: func(ctx context.Context, body []byte) (*http.Request, error) {
						return http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/responses", strings.NewReader(string(body)))
					},
					ApplyHeaders:   func(http.Header) {},
					Latency:        func(time.Duration) {},
					TransportError: func(err error) error { return err },
					Do: func(*http.Request) (*http.Response, error) {
						if len(bodies) > 0 {
							require.GreaterOrEqual(t, bodies[len(bodies)-1].closes, 1, "prior attempt body must close before next exchange")
						}
						b := &trackedBody{Reader: strings.NewReader(`{"error":{"message":"compact rejected"}}`)}
						bodies = append(bodies, b)
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: b}, nil
					},
				},
				NonStreamOptions: func() openai.NonStreamOptions {
					return openai.NonStreamOptions{
						ReadBody: func(io.Reader) ([]byte, error) {
							return nil, &compact.Failure{
								Message: "compact unsupported",
								Payload: []byte(`{"error":{"message":"compact unsupported"}}`),
							}
						},
					}
				},
				CompactRetry: func(_ []byte, _ int, _ string, _ []byte, tried bool) ([]byte, string, bool) {
					if tried {
						return nil, "", false
					}
					return []byte(`{"model":"gpt-5.1-codex"}`), "gpt-5.1-codex", true
				},
				CompactRetryObserved: func(*http.Response, []byte, string) {
					observedRetries++
				},
				ObserveUpstreamModel: func(model string) {
					observedModels = append(observedModels, model)
				},
				CompactErrorResponse: func(_ *http.Response, signal CompactFailure) (*http.Response, []byte) {
					return &http.Response{
						StatusCode: http.StatusBadRequest,
						Header:     make(http.Header),
					}, signal.Payload
				},
				ShouldFailover: func(int, string, []byte) bool {
					return failoverEligible
				},
				CompactFailover: func(*http.Response, []byte, string, string) error {
					return failoverErr
				},
				ErrorResponse: func(_ *http.Response, _ []byte, model string) error {
					presenterModel = model
					return presenterErr
				},
			}

			result, err := RunHTTP(context.Background(), HTTPInput{
				Body:          []byte(`{"model":"gpt-5"}`),
				BillingModel:  "gpt-5-billing",
				UpstreamModel: "gpt-5",
			}, options)
			require.Nil(t, result)
			require.Len(t, bodies, 2)
			require.Equal(t, 1, observedRetries)
			require.Equal(t, []string{"gpt-5.1-codex"}, observedModels)
			for i, b := range bodies {
				require.GreaterOrEqual(t, b.closes, 1, "attempt %d body must close", i)
			}
			if failoverEligible {
				require.ErrorIs(t, err, failoverErr)
			} else {
				require.ErrorIs(t, err, presenterErr)
				require.Equal(t, "gpt-5.1-codex", presenterModel)
			}
		})
	}
}
