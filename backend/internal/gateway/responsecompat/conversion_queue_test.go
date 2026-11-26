package responsecompat

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// conversion_queue_test.go uses a virtual clock to separate snapshot
// replacement from cancellation while every aggregation slot is occupied.
// The caller can report the latest evaluated revision without writing over
// the revision used for initial admission.

// TestConversionQueueRetainsLastEvaluatedRevision cancels a waiter after a
// new enforcing policy reaches it. The caller receives the new revision
// without a slot, so it can finalize the original cohort accurately.
func TestConversionQueueRetainsLastEvaluatedRevision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service := New(nil)
		tuple := Tuple{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"}
		rules := map[Tuple]Rule{tuple: {Tuple: tuple, Mode: Enforce, SyncToStream: true, EnforcePercent: 100}}
		require.NoError(t, service.publish(&snapshot{revision: 1, mode: Enforce, rules: rules}))
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		for range maxConcurrentConversions {
			release, observed, err := service.AcquireConversion(ctx, tuple, "holder", nil)
			require.NoError(t, err)
			require.EqualValues(t, 1, observed.Revision)
			defer release()
		}
		stopped := make(chan Decision, 1)
		go func() {
			release, observed, err := service.AcquireConversion(ctx, tuple, "waiter", nil)
			if release != nil {
				release()
				t.Error("full queue granted another slot")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("expected cancellation, got %v", err)
			}
			stopped <- observed
		}()
		// Confirm the waiter has parked under each revision. Cancellation then
		// exercises finalization after observing version two, with capacity still full.
		synctest.Wait()
		require.NoError(t, service.publish(&snapshot{revision: 2, mode: Enforce, rules: rules}))
		synctest.Wait()
		cancel()
		require.Equal(t, Decision{Mode: Enforce, Revision: 2}, <-stopped)
	})
}
