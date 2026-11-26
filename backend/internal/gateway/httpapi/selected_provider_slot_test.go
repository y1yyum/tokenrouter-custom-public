package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/TokenFlux/TokenRouter/internal/gateway/httpapi/testkit"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// responsesSlotCache enforces one physical slot and exposes queue admission.
type responsesSlotCache struct {
	testkit.ConcurrencyHooks               // unrelated concurrency operations
	occupied                 atomic.Bool   // physical provider slot occupancy
	releases                 atomic.Int32  // physical provider release calls
	waiting                  chan struct{} // registrations on the provider queue
}

// responsesSlotSticky accepts affinity updates without affecting slot ownership.
type responsesSlotSticky struct{}

// responsesSlotBody holds the paused reader open after transport cancellation.
type responsesSlotBody struct {
	io.ReadCloser                 // real HTTP body
	interrupted   chan struct{}   // network read has returned
	resume        <-chan struct{} // allows the reader to return
	closed        chan struct{}   // actual response body closure
}

// TestSelectedProviderSlotOwnsOnlyAcquiredResource supplies an already acquired
// selection with a release counter and no sticky service. The acquired hook
// runs once and repeated returned releases invoke underlying cleanup once.
func TestSelectedProviderSlotOwnsOnlyAcquiredResource(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	var released atomic.Int32
	acquired := 0
	hooks := ProviderSlotHooks{Acquired: func(*gin.Context) { acquired++ }, CapacityLimited: func(*gin.Context) { t.Fatal("unexpected capacity failure") }}
	started := false
	release, ok := AcquireSelectedProviderSlot(c, nil, "session", &SelectedProviderSlot{ProviderID: 12, Acquired: true, ReleaseFunc: func() { released.Add(1) }}, false, &started, zap.NewNop(), func(int, string, string, string) { t.Fatal("unexpected HTTP error") }, nil, nil, hooks)
	require.True(t, ok)
	require.Equal(t, 1, acquired)
	require.Zero(t, released.Load())
	release()
	release()
	require.EqualValues(t, 1, released.Load())
}

// TestSelectedProviderSlotMissingWaitPlanKeepsErrorShape supplies a selection
// with no acquired slot or wait plan. It requires the capacity observation and
// unchanged 503 error category, message and empty code.
func TestSelectedProviderSlotMissingWaitPlanKeepsErrorShape(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	limited := false
	status := 0
	kind, message := "", ""
	started := false
	release, ok := AcquireSelectedProviderSlot(c, nil, "", &SelectedProviderSlot{ProviderID: 12}, false, &started, zap.NewNop(), func(s int, k, code, m string) { status = s; kind = k; message = m; require.Empty(t, code) }, nil, nil, ProviderSlotHooks{Acquired: func(*gin.Context) { t.Fatal("unexpected acquired") }, CapacityLimited: func(*gin.Context) { limited = true }})
	require.False(t, ok)
	require.Nil(t, release)
	require.True(t, limited)
	require.Equal(t, 503, status)
	require.Equal(t, "api_error", kind)
	require.Equal(t, "No available providers", message)
}

// TestSelectedProviderSlotRetainsResponsesDrain runs the three slot acquisition
// paths against one TCP receiver. A second request cannot reach that receiver
// while the first is draining or its interrupted reader still owns the slot.
func TestSelectedProviderSlotRetainsResponsesDrain(t *testing.T) {
	for _, mode := range []string{"prefetched", "immediate", "queued"} {
		t.Run(mode, func(t *testing.T) {
			client, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			first, disconnect := context.WithCancel(client)
			defer disconnect()
			tag := "X-Drain-Request"
			var arrivals atomic.Int32
			bodyClosed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrivals.Add(1)
				if r.Header.Get(tag) == "A" {
					w.Header().Set("Content-Type", "text/event-stream")
					if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
						t.Errorf("write upstream heartbeat: %v", err)
						return
					}
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Errorf("flush upstream heartbeat: %v", err)
						return
					}
					<-r.Context().Done()
					return
				}
				select {
				case <-bodyClosed:
				default:
					t.Error("request B arrived before request A closed its response body")
				}
			}))
			defer upstream.Close()

			cache := &responsesSlotCache{waiting: make(chan struct{}, 2)}
			concurrency := NewConcurrencyHelper(scheduler.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
			getSlot := func(ctx context.Context, selection *SelectedProviderSlot) (func(), bool) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", nil)
				started := false
				hooks := ProviderSlotHooks{Acquired: func(*gin.Context) {}, CapacityLimited: func(*gin.Context) {}}
				return AcquireSelectedProviderSlot(c, nil, "", selection, false, &started, zap.NewNop(),
					func(status int, _, _, message string) { t.Errorf("slot error %d: %s", status, message) },
					concurrency, responsesSlotSticky{}, hooks)
			}
			plan := &scheduler.ProviderWaitPlan{MaxConcurrency: 1, MaxWaiting: 2, Timeout: 5 * time.Second}
			selection := &SelectedProviderSlot{ProviderID: 57, WaitPlan: plan, CompleteBeforeRelease: true}
			var releaseBlocker func()
			if mode != "immediate" {
				held, ok, err := concurrency.TryAcquireProviderSlot(client, 57, 1)
				require.NoError(t, err)
				require.True(t, ok)
				if mode == "prefetched" {
					selection.Acquired = true
					selection.ReleaseFunc = held
				} else {
					releaseBlocker = held
					defer releaseBlocker()
				}
			}

			reading := make(chan struct{})
			interrupted := make(chan struct{})
			allowReadExit, finishReadGate := context.WithCancel(client)
			defer finishReadGate()
			aDone := make(chan error, 1)
			go func() {
				release, ok := getSlot(first, selection)
				if !ok {
					aDone <- io.ErrClosedPipe
					return
				}
				defer release()
				defer release()
				req, err := http.NewRequestWithContext(first, http.MethodPost, upstream.URL, http.NoBody)
				if err != nil {
					aDone <- err
					return
				}
				req.Header.Set(tag, "A")
				attempt := openai.ResponsesStreamAttempt{Transport: openai.ResponsesTransportOptions{
					StartedAt: time.Now(), Deadline: time.Now().Add(4 * time.Second),
					FirstOutputTimeout: 3 * time.Second, StreamInterval: 3 * time.Second, DrainTimeout: 180 * time.Millisecond,
				}}
				resp, err := attempt.Exchange(first, req, func(req *http.Request) (*http.Response, error) {
					resp, err := upstream.Client().Do(req)
					if err == nil {
						resp.Body = &responsesSlotBody{
							ReadCloser: resp.Body, interrupted: interrupted, resume: allowReadExit.Done(), closed: bodyClosed,
						}
					}
					return resp, err
				})
				if err != nil {
					aDone <- err
					return
				}
				close(reading)
				_, err = io.Copy(io.Discard, resp.Body)
				closeErr := resp.Body.Close()
				if closeErr != nil {
					t.Errorf("close A: %v", closeErr)
				}
				aDone <- err
			}()
			if releaseBlocker != nil {
				waitResponsesSlotSignal(t, client, cache.waiting)
				releaseBlocker()
			}
			waitResponsesSlotSignal(t, client, reading)
			releasedBeforeDrain := cache.releases.Load()
			disconnect()

			bDone := make(chan error, 1)
			go func() {
				release, ok := getSlot(client, &SelectedProviderSlot{ProviderID: 57, WaitPlan: plan, CompleteBeforeRelease: true})
				if !ok {
					bDone <- io.ErrClosedPipe
					return
				}
				defer release()
				req, err := http.NewRequestWithContext(client, http.MethodPost, upstream.URL, http.NoBody)
				if err != nil {
					bDone <- err
					return
				}
				resp, err := upstream.Client().Do(req)
				if err == nil {
					err = resp.Body.Close()
				}
				bDone <- err
			}()
			waitResponsesSlotSignal(t, client, cache.waiting)
			require.EqualValues(t, 1, arrivals.Load())
			waitResponsesSlotSignal(t, client, interrupted)
			require.EqualValues(t, 1, arrivals.Load())
			require.Equal(t, releasedBeforeDrain, cache.releases.Load())
			require.True(t, cache.occupied.Load())
			finishReadGate()
			require.ErrorIs(t, <-aDone, context.Canceled)
			require.NoError(t, <-bDone)
			require.EqualValues(t, 2, arrivals.Load())
			require.Eventually(t, func() bool { return cache.releases.Load() == releasedBeforeDrain+2 }, time.Second, time.Millisecond)
		})
	}
}

// waitResponsesSlotSignal bounds synchronization with a reader or queue event.
func waitResponsesSlotSignal(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the slot scenario")
	}
}

// AcquireProviderSlot grants capacity only when the fixture's slot is vacant.
func (c *responsesSlotCache) AcquireProviderSlot(ctx context.Context, _ int64, _ int, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return c.occupied.CompareAndSwap(false, true), nil
}

// ReleaseProviderSlot makes the fixture's single slot available to queued attempts.
func (c *responsesSlotCache) ReleaseProviderSlot(context.Context, int64, string) error {
	c.releases.Add(1)
	c.occupied.Store(false)
	return nil
}

// IncrementProviderWaitCount publishes actual scheduler queue registration.
func (c *responsesSlotCache) IncrementProviderWaitCount(context.Context, int64, int) (bool, error) {
	c.waiting <- struct{}{}
	return true, nil
}

// BindStickySession satisfies selected-slot acquisition without a storage write.
func (responsesSlotSticky) BindStickySession(context.Context, *int64, string, int64) error {
	return nil
}

// Read pauses on network exit so the test can observe the still-held slot.
// The underlying HTTP body allows cancellation and closure to proceed concurrently.
func (r *responsesSlotBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil {
		close(r.interrupted)
		<-r.resume
	}
	return n, err
}

// Close publishes explicit body closure after the actual network body closes.
func (r *responsesSlotBody) Close() error {
	err := r.ReadCloser.Close()
	close(r.closed)
	return err
}
