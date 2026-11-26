package responsecompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// service.go persists versioned Responses compatibility settings and publishes
// a local snapshot for candidate matching. Administrators compare the revision
// they read, while storage compares the full raw value to protect updates from
// other instances. The settings session serializes writes and publication
// within this process.
//
// Only committed values may enter the local snapshot. Administrator readback
// reports the saved revision separately from the revision applied here, so a
// saved value can be visible even if its publication failed. Candidate matching
// takes one atomic load and never waits for a settings read or update.

const (
	// SettingKey stores the Responses rollout independently of other gateway controls.
	SettingKey = "gateway_responses_request_compatibility"
)

// errRevisionConflict tells an administrator to read the latest settings before updating.
var errRevisionConflict = apperror.Conflict("RESPONSES_COMPATIBILITY_CONFLICT", "Compatibility settings changed. Reload before saving.")

// Status exposes the database configuration and the revision in effect on the
// serving instance. AppliedRevision is nil until this instance has published
// a snapshot. In a concurrent read, the applied revision can be newer than Settings.
type Status struct {
	Settings        Config `json:"settings"`         // saved configuration or revision zero defaults
	AppliedRevision *int64 `json:"applied_revision"` // revision applied on this instance
	Fresh           bool   `json:"fresh"`            // local snapshot is still inside its send-time lifetime
}

// Service owns the stored policy and its immutable local snapshot.
// @project-doc docs/interfaces/configuration.md#responses_request_compatibility
type Service struct {
	store           *settings.Store          // settings persistence and update coordination
	applied         atomic.Pointer[snapshot] // latest successfully published configuration
	generation      atomic.Uint64            // monotonically increasing local authority operation
	conversionSlots chan struct{}            // shared by every tuple served by this instance
}

// New creates a service whose local rollout starts off until publication.
func New(store *settings.Store) *Service {
	return &Service{store: store, conversionSlots: make(chan struct{}, maxConcurrentConversions)}
}

// MayMatch reports whether a fresh local policy can admit a candidate. Callers
// can skip inspecting a request body when this returns false and the request
// has no prior admission to restore.
func (s *Service) MayMatch() bool {
	current := s.activeSnapshot()
	return current != nil && current.mode != Off
}

// Match decides the arm for a fully mapped outbound candidate using local data
// only. A missing snapshot is off, including when the gateway fixture supplies
// no service. The caller checks protocol eligibility and retains first admission
// across retries before using a later decision to authorize a conversion.
func (s *Service) Match(tuple Tuple, logicalRequestID string) Decision {
	current := s.activeSnapshot()
	if current == nil {
		return Decision{Mode: Off}
	}
	return current.decide(tuple, logicalRequestID)
}

// activeSnapshot returns the fresh policy, including explicitly disabled revisions.
func (s *Service) activeSnapshot() *snapshot {
	if s == nil {
		return nil
	}
	current := s.applied.Load()
	if current == nil || time.Since(current.readStarted) >= snapshotLifetime {
		return nil
	}
	return current
}

// Get reads the saved policy without changing the local rollout decision.
// Absent settings resolve to revision zero. SQL and invalid storage errors
// remain visible to the administrator.
func (s *Service) Get(ctx context.Context) (Status, error) {
	config, _, err := s.read(ctx)
	if err != nil {
		return Status{}, err
	}
	return s.status(config), nil
}

// Update replaces the entire policy if the submitted revision is still current.
// It assigns the next revision and only publishes after the raw-value CAS
// commits. Persistence failures leave the local snapshot intact. A post-commit
// publication failure returns the coordinator's persisted=true metadata.
func (s *Service) Update(ctx context.Context, replacement Config) (Status, error) {
	if replacement.Revision < 0 || replacement.Revision == math.MaxInt64 {
		return Status{}, apperror.BadRequest("INVALID_RESPONSES_COMPATIBILITY", "Revision must allow a subsequent version.")
	}
	expectedRevision := replacement.Revision
	replacement.Revision++
	config, next, err := compile(replacement)
	if err != nil {
		return Status{}, apperror.BadRequest("INVALID_RESPONSES_COMPATIBILITY", err.Error())
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return Status{}, err
	}
	var readStarted time.Time
	err = s.store.ReplaceVersioned(ctx, SettingKey, string(raw), expectedRevision, errRevisionConflict,
		func(ctx context.Context) (int64, *string, error) {
			readStarted = time.Now()
			current, expected, err := s.read(ctx)
			return current.Revision, expected, err
		},
		func(context.Context) error {
			// Notify subscribers only once the committed snapshot is readable;
			// callbacks may inspect application state while handling the notification.
			if err := s.publishAt(next, readStarted, s.generation.Add(1)); err != nil {
				return err
			}
			s.store.NotifyUpdated()
			return nil
		},
	)
	if err != nil {
		return Status{}, err
	}
	return s.status(config), nil
}

// read returns the parsed policy and the exact CAS expectation. An absent key
// expects an absent row, normally before the initial administrator save.
func (s *Service) read(ctx context.Context) (Config, *string, error) {
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	value, err := s.store.GetValue(readCtx, SettingKey)
	if readCtx.Err() != nil {
		return Config{}, nil, readCtx.Err()
	}
	if errors.Is(err, settings.ErrSettingNotFound) {
		// Missing storage is the off configuration at revision zero, allowing
		// an initial CAS without a separate database initialization write.
		return Config{Mode: Off, Rules: []Rule{}}, nil, nil
	}
	if err != nil {
		return Config{}, nil, err
	}
	var config Config
	if err := json.Unmarshal([]byte(value), &config); err != nil || config.Revision <= 0 {
		return Config{}, nil, apperror.InternalServer("INVALID_STORED_RESPONSES_COMPATIBILITY", "Stored compatibility settings are invalid.")
	}
	normalized, _, err := compile(config)
	if err != nil {
		return Config{}, nil, apperror.InternalServer("INVALID_STORED_RESPONSES_COMPATIBILITY", "Stored compatibility settings are invalid.").WithCause(err)
	}
	return normalized, &value, nil
}

// publish exposes a validated version without allowing an older publication
// to undo a newer one. Equal revisions refer to the same committed update.
func (s *Service) publish(next *snapshot) error {
	return s.publishAt(next, time.Now(), s.generation.Add(1))
}

// publishAt installs the snapshot with its authority-read start time. Later
// generations may renew an equal revision but a delayed result cannot renew
// freshness after a newer result or restore an obsolete persisted revision.
func (s *Service) publishAt(next *snapshot, started time.Time, generation uint64) error {
	next.readStarted = started
	next.generation = generation
	next.changed = make(chan struct{})
	for {
		current := s.applied.Load()
		if current != nil && current.revision > next.revision {
			return fmt.Errorf("compatibility revision %d is older than applied revision %d", next.revision, current.revision)
		}
		if current != nil && current.generation >= generation {
			return nil
		}
		// Swap the revision and its indexed rules together so a concurrent
		// candidate can see only one complete configuration at a time.
		if s.replaceSnapshot(current, next) {
			return nil
		}
	}
}

// status pairs the requested saved configuration with this instance's observed
// applied revision. The caller can compare the two versions after any write.
func (s *Service) status(config Config) Status {
	result := Status{Settings: config}
	if current := s.applied.Load(); current != nil {
		revision := current.revision
		result.AppliedRevision = &revision
		result.Fresh = time.Since(current.readStarted) < snapshotLifetime
	}
	return result
}
