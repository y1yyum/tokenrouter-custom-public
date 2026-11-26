package responsecompat

import (
	"context"
	"time"
)

// conversion_queue.go limits concurrent response aggregation across one
// instance. Each rule competes for the same four slots because the underlying
// readers allocate from the same process heap. A forwarding attempt yields
// any provider lease before blocking for local memory. Once sent, it returns
// the slot after the reader exits and the upstream body closes.
//
// A waiter observes the exact snapshot that permitted entry. Publication
// signals replacement after the atomic swap, and a separate timer covers
// expiration when the settings reader cannot refresh. Either notification
// prompts a new policy evaluation while the original request cutoff remains
// in force. Withdrawal can therefore resume a synchronous send while all
// four current readers continue their exchanges.

const (
	// maxConcurrentConversions caps Responses aggregators sharing the process heap across rules and providers.
	maxConcurrentConversions = 4
)

// AcquireConversion reserves aggregation capacity for an already enrolled
// enforce candidate. It returns no release when the policy stops permitting
// conversion. Before blocking it invokes yieldProvider once, so the caller
// can surrender any prefetched provider capacity. The caller supplies a
// bounded context and invokes the returned release once after exchange cleanup.
// A slot grant can coincide with cancellation or policy withdrawal, so the
// caller checks both again before any upstream send. The returned decision is
// the last policy observed, or empty if cancellation preceded any evaluation.
func (s *Service) AcquireConversion(ctx context.Context, tuple Tuple, requestID string, yieldProvider func()) (func(), Decision, error) {
	var observed Decision
	for {
		if err := ctx.Err(); err != nil {
			return nil, observed, err
		}
		current := s.activeSnapshot()
		observed = current.decide(tuple, requestID)
		if observed.Mode != Enforce {
			return nil, observed, nil
		}
		select {
		case s.conversionSlots <- struct{}{}:
			return s.releaseConversion, observed, nil
		default:
		}
		if yieldProvider != nil {
			// Never block on the local slot while holding distributed provider
			// capacity that another request may need to finish its exchange.
			yieldProvider()
			yieldProvider = nil
		}
		if s.waitForConversion(ctx, current) {
			return s.releaseConversion, observed, nil
		}
	}
}

// waitForConversion waits on the same snapshot used for the admission check.
// A replacement or expiration sends the caller back through policy evaluation
// without requiring a slot to be returned or resetting the request budget.
func (s *Service) waitForConversion(ctx context.Context, current *snapshot) bool {
	expiry := time.NewTimer(time.Until(current.readStarted.Add(snapshotLifetime)))
	defer expiry.Stop()
	select {
	case s.conversionSlots <- struct{}{}:
		return true
	case <-current.changed:
	case <-expiry.C:
	case <-ctx.Done():
	}
	return false
}

// releaseConversion makes one slot available after the current exchange finishes.
// Only the owner of a successful grant calls it, once, after cleanup or withdrawal.
func (s *Service) releaseConversion() { <-s.conversionSlots }

// replaceSnapshot wakes waiters only after they can read the replacement.
// Only the successful swap can retire its predecessor, so overlapping
// authority readers cannot close the same notification channel twice.
// Waiters already holding the old snapshot still have its notification
// channel when they start waiting, even when the swap has already completed.
func (s *Service) replaceSnapshot(current, next *snapshot) bool {
	if !s.applied.CompareAndSwap(current, next) {
		return false
	}
	if current != nil {
		close(current.changed)
	}
	return true
}
