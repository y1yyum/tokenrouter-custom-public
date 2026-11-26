package responsecompat

import (
	"context"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
)

// refresh.go bounds the lifetime of the local Responses conversion policy.
// A reader starts its freshness clock before the database call, so queued
// database work cannot make an old revision look freshly confirmed. HTTP
// forwarding reads the atomic snapshot and never waits for this poller.

const (
	refreshInterval  = time.Second     // polling cadence for each application instance
	readTimeout      = time.Second     // maximum duration of one authoritative read
	snapshotLifetime = 3 * time.Second // maximum age accepted before a new send
)

// Refresh reads the authority using a bounded context and replaces the local
// policy on success. Transient failures retain the original expiration time;
// an invalid stored value immediately withdraws the last valid policy.
func (s *Service) Refresh(ctx context.Context) error {
	started := time.Now()
	generation := s.generation.Add(1)
	readCtx, cancel := context.WithDeadline(ctx, started.Add(readTimeout))
	defer cancel()
	config, _, err := s.read(readCtx)
	if contextErr := readCtx.Err(); contextErr != nil {
		return contextErr
	}
	if time.Since(started) >= readTimeout {
		return context.DeadlineExceeded
	}
	if err != nil {
		if apperror.Reason(err) == "INVALID_STORED_RESPONSES_COMPATIBILITY" {
			s.withdraw(generation)
		}
		return err
	}
	_, next, err := compile(config)
	if err != nil {
		s.withdraw(generation)
		return err
	}
	if err := s.publishAt(next, started, generation); err != nil {
		// A missing or restored row may have a lower revision than this process
		// already applied. Stop serving that policy until authority catches up.
		s.withdraw(generation)
		return err
	}
	return nil
}

// withdraw stops conversion without forgetting the revision high-water mark.
// Older reads that complete after this observation cannot make it fresh again.
func (s *Service) withdraw(generation uint64) {
	for {
		old := s.applied.Load()
		if old != nil && old.generation >= generation {
			return
		}
		off := &snapshot{mode: Off, generation: generation, changed: make(chan struct{})}
		if old != nil {
			off.revision = old.revision
		}
		if s.replaceSnapshot(old, off) {
			return
		}
	}
}

// Run refreshes at startup and on a fixed cadence until its owner cancels
// the context. A read failure cannot extend the lifetime checked at send time.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		_ = s.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
