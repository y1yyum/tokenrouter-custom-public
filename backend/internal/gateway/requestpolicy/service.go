package requestpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// service.go owns the independently saved parameter policy and its local
// entry snapshot. Administrators replace the entire policy at an explicit
// revision using the shared settings coordinator. Request admission takes its
// decision from one published snapshot and stores the result in the logical
// request context before any parameter validations or rewrites run.
//
// Each instance polls the saved value, and successful authority reads have a
// limited lifetime. An invalid value or expired snapshot stops new enrollment.
// The timestamp orders overlapping publications, while the revision protects
// against a newer authority read moving back to an older saved version.

const (
	// SettingKey identifies the independently persisted cross-protocol policy.
	SettingKey = "gateway_request_parameter_policy"

	// Authority checks age local policy and bound consecutive periods of failed reads.
	refreshInterval  = time.Second
	readTimeout      = time.Second
	snapshotLifetime = 3 * time.Second
)

var (
	// Setting errors distinguish competing saves from invalid stored administrator state.
	errConflict      = apperror.Conflict("REQUEST_PARAMETER_POLICY_CONFLICT", "Parameter policy changed. Reload before saving.")
	errInvalidStored = apperror.InternalServer("INVALID_STORED_REQUEST_PARAMETER_POLICY", "Stored parameter policy is invalid.")
)

// Status reports persisted settings and the instance's latest applied revision.
type Status struct {
	Settings        Config `json:"settings"`         // policy read from storage
	AppliedRevision *int64 `json:"applied_revision"` // nil before a valid publication
	Fresh           bool   `json:"fresh"`            // current instance snapshot validity
}

// snapshot publishes the rule index, revision, and freshness clock together.
type snapshot struct {
	readStarted time.Time      // start of authority read
	revision    int64          // applied revision high-water mark
	mode        Mode           // global rollout cap
	rules       map[Entry]Rule // exact match index
	valid       bool           // authority value passed validation
}

// Service owns one database key and the local entry-policy snapshot.
// @project-doc docs/interfaces/configuration.md#request_parameter_policy
type Service struct {
	store   *settings.Store          // shared runtime settings store
	applied atomic.Pointer[snapshot] // latest local authority observation
}

// New constructs an inactive entry policy backed by the shared settings store.
func New(store *settings.Store) *Service { return &Service{store: store} }

// Admit binds a matched rule to the request before the caller validates fields
// or rewrites the body. A false result leaves the supplied context unchanged.
// If admission already happened, the original context and decision win even
// after the service has published a replacement or the group has changed.
func (s *Service) Admit(ctx context.Context, entryGroupID int64, endpoint, requestID string, raw []byte) (context.Context, bool) {
	if _, ok := FromContext(ctx); ok {
		return ctx, true
	}
	if s == nil || entryGroupID <= 0 || requestID == "" {
		return ctx, false
	}
	current := s.applied.Load()
	if !current.fresh() || current.mode == ModeOff {
		return ctx, false
	}
	model := gjson.GetBytes(raw, "model")
	if model.Type != gjson.String || model.String() == "" {
		return ctx, false
	}
	entry := Entry{EntryGroupID: entryGroupID, Endpoint: endpoint, RequestedModel: model.String()}
	rule, matched := current.rules[entry]
	if !matched || rule.Mode == ModeOff {
		return ctx, false
	}
	mode := ModeObserve
	if current.mode == ModeStrict && rule.Mode == ModeStrict && rolloutBucket(requestID) < rule.StrictPercent {
		mode = ModeStrict
	}
	return withIntent(ctx, mode, current.revision, entry, raw), true
}

// Get reads the persisted policy and reports this instance's publication state.
// A missing value is revision zero and off. Invalid stored data is surfaced
// to the caller so storage problems remain distinct from a disabled policy.
func (s *Service) Get(ctx context.Context) (Status, error) {
	config, _, _, err := s.read(ctx)
	if err != nil {
		return Status{}, err
	}
	return s.status(config), nil
}

// Update validates and replaces the policy at the caller's expected revision.
// The shared coordinator publishes only after persistence, and a post-commit
// application failure is reported with its persisted status to the administrator.
func (s *Service) Update(ctx context.Context, replacement Config) (Status, error) {
	if replacement.Revision < 0 || replacement.Revision == math.MaxInt64 {
		return Status{}, apperror.BadRequest("INVALID_REQUEST_PARAMETER_POLICY", "Revision must allow a subsequent version.")
	}
	expected := replacement.Revision
	replacement.Revision++
	config, rules, err := compile(replacement)
	if err != nil {
		return Status{}, apperror.BadRequest("INVALID_REQUEST_PARAMETER_POLICY", err.Error())
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return Status{}, err
	}
	var started time.Time
	err = s.store.ReplaceVersioned(ctx, SettingKey, string(raw), expected, errConflict,
		func(ctx context.Context) (int64, *string, error) {
			started = time.Now()
			current, _, raw, err := s.read(ctx)
			return current.Revision, raw, err
		},
		func(context.Context) error {
			// Notify after publishing the committed snapshot so local subscribers
			// can inspect the same setting as new HTTP requests during the callback.
			if err := s.publish(started, config, rules, true); err != nil {
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

// Refresh renews the entry snapshot from a bounded authoritative read. An
// invalid result withdraws new admissions immediately, while a transient read
// failure leaves the preceding freshness clock to expire on its own.
func (s *Service) Refresh(ctx context.Context) error {
	started := time.Now()
	config, rules, _, err := s.read(ctx)
	if apperror.Reason(err) == apperror.Reason(errInvalidStored) {
		_ = s.publish(started, Config{}, nil, false)
	}
	if err != nil {
		return err
	}
	return s.publish(started, config, rules, true)
}

// Run polls independently of Responses compatibility until the application
// owner cancels the context. Read errors leave freshness enforcement with
// Refresh and Admit, which never renew a snapshot on a failed read.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		// A failed poll leaves the current expiration clock intact and the
		// next tick retries the authority read without delaying entry traffic.
		_ = s.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// read returns the validated settings, match index, and exact CAS expectation.
// A missing key is the initial off mode and uses nil as the storage expectation.
// A result completed after its read deadline cannot renew the ingress snapshot.
func (s *Service) read(ctx context.Context) (Config, map[Entry]Rule, *string, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	raw, err := s.store.GetValue(ctx, SettingKey)
	if ctx.Err() != nil {
		return Config{}, nil, nil, ctx.Err()
	}
	if errors.Is(err, settings.ErrSettingNotFound) {
		return Config{Mode: ModeOff, Rules: []Rule{}}, nil, nil, nil
	}
	if err != nil {
		return Config{}, nil, nil, err
	}
	var config Config
	if err := json.Unmarshal([]byte(raw), &config); err != nil || config.Revision <= 0 {
		return Config{}, nil, nil, errInvalidStored
	}
	config, index, err := compile(config)
	if err != nil {
		return Config{}, nil, nil, errInvalidStored.WithCause(err)
	}
	return config, index, &raw, nil
}

// publish swaps in the latest authoritative observation and its complete index.
// An older read at an equal revision cannot renew freshness. Greater revisions
// from committed writes can still advance the snapshot even when another read
// started later but finished before that commit. A lower revision observed by
// a newer read invalidates the snapshot and keeps the prior revision watermark.
func (s *Service) publish(started time.Time, config Config, rules map[Entry]Rule, valid bool) error {
	for {
		old := s.applied.Load()
		if old != nil && !started.After(old.readStarted) && (!valid || config.Revision <= old.revision) {
			return nil
		}
		next := &snapshot{readStarted: started, revision: config.Revision, mode: config.Mode, rules: rules, valid: valid}
		var err error
		if old != nil && (!valid || config.Revision < old.revision) {
			if valid {
				err = fmt.Errorf("parameter policy revision %d is older than applied revision %d", config.Revision, old.revision)
			}
			next = &snapshot{readStarted: started, revision: old.revision}
		}
		// Publish the index alongside its authority metadata so admission never
		// combines rules from one revision with another revision's rollout mode.
		if s.applied.CompareAndSwap(old, next) {
			return err
		}
	}
}

// status pairs the saved configuration with the observed instance revision.
func (s *Service) status(config Config) Status {
	current := s.applied.Load()
	result := Status{Settings: config, Fresh: current.fresh()}
	if current != nil && (current.valid || current.revision > 0) {
		revision := current.revision
		result.AppliedRevision = &revision
	}
	return result
}

// fresh reports whether this instance can use the snapshot for new admissions.
func (s *snapshot) fresh() bool {
	return s != nil && s.valid && time.Since(s.readStarted) < snapshotLifetime
}
