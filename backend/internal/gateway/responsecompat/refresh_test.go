package responsecompat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// refresh_test.go exercises the send-side freshness boundary and reads that
// finish out of order. Both callers use the same persisted policy but hold
// independent local snapshots; no notification channel connects the instances.

// pausedCompatibilityRead returns the value captured before the test releases
// its barrier. Ignoring cancellation lets the test exercise a late SQL result.
type pausedCompatibilityRead struct {
	settings.Repository
	value   string
	started chan struct{}
	release chan struct{}
}

// GetValue blocks after capturing the test's immutable policy revision.
func (p *pausedCompatibilityRead) GetValue(context.Context, string) (string, error) {
	close(p.started)
	<-p.release
	return p.value, nil
}

// TestResponsesCompatibilityRefresh checks polling, invalid configuration,
// and deadline expiry without relying on cross-instance notifications.
func TestResponsesCompatibilityRefresh(t *testing.T) {
	repo := &compatibilityRepository{}
	first, second := New(settings.New(repo)), New(settings.New(repo))
	tuple := Tuple{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"}
	config := Config{Mode: Enforce, Rules: []Rule{{Tuple: tuple, Mode: Enforce, SyncToStream: true, EnforcePercent: 100}}}
	_, err := first.Update(t.Context(), config)
	require.NoError(t, err)
	require.NoError(t, second.Refresh(t.Context()))
	require.Equal(t, Enforce, second.Match(tuple, "logical").Mode)
	before := repo.value
	config.Mode, config.Revision = Off, 1
	_, err = first.Update(t.Context(), config)
	require.NoError(t, err)
	require.NoError(t, second.Refresh(t.Context()))
	require.Equal(t, Off, second.Match(tuple, "logical").Mode)
	repo.value = before
	require.Error(t, second.Refresh(t.Context()))
	require.Equal(t, Off, second.Match(tuple, "logical").Mode)

	late := &pausedCompatibilityRead{value: before, started: make(chan struct{}), release: make(chan struct{})}
	instance := New(settings.New(late))
	done := make(chan error, 1)
	go func() { done <- instance.Refresh(t.Context()) }()
	<-late.started
	require.NoError(t, instance.publishAt(&snapshot{mode: Off, revision: 2}, time.Now(), instance.generation.Add(1)))
	close(late.release)
	require.Error(t, <-done)
	require.Equal(t, Off, instance.Match(tuple, "logical").Mode)

	// A timely DB response may renew a revision; the previous start time
	// still controls eligibility if the next read cannot confirm that revision.
	_, stale, err := compile(Config{Revision: 3, Mode: Enforce, Rules: config.Rules})
	require.NoError(t, err)
	require.NoError(t, instance.publishAt(stale, time.Now().Add(-snapshotLifetime), instance.generation.Add(1)))
	require.Equal(t, Off, instance.Match(tuple, "logical").Mode)
	repo.value = "{"
	require.Error(t, first.Refresh(t.Context()))
	require.Equal(t, Off, first.Match(tuple, "logical").Mode)
}

// TestResponsesCompatibilitySlowRead rejects a completed database read after
// its deadline even when the repository ignores the cancellation signal.
func TestResponsesCompatibilitySlowRead(t *testing.T) {
	late := &pausedCompatibilityRead{value: `{"revision":1,"mode":"enforce","rules":[]}`, started: make(chan struct{}), release: make(chan struct{})}
	instance := New(settings.New(late))
	done := make(chan error, 1)
	go func() { done <- instance.Refresh(t.Context()) }()
	<-late.started
	time.Sleep(readTimeout + 10*time.Millisecond)
	close(late.release)
	require.True(t, errors.Is(<-done, context.DeadlineExceeded))
	require.Nil(t, instance.applied.Load())
}
