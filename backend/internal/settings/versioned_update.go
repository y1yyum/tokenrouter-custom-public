package settings

import (
	"context"
	"errors"

	"github.com/TokenFlux/TokenRouter/internal/pkg/locale"
)

// versioned_update.go coordinates replacement of a single independently
// versioned setting. The application session keeps the authority read, write,
// and publication in order within this process. Other instances use their own
// sessions, so a database compare-and-swap also compares the exact stored text.
// The two checks address different writers and run before the policy becomes
// visible to request handlers.
//
// A conflict uses the caller's setting-specific error. Other failures keep
// their original metadata, including the distinction between a failed save and
// a save that committed but could not be applied locally.

// ReplaceVersioned updates one row after comparing the caller's revision.
// Read returns the current revision and the exact storage expectation. A nil
// expectation is for a missing value. Apply publishes the validated replacement
// only after the database commits, so a losing writer cannot expose its own
// configuration. The replacement value must already contain its next revision.
func (s *Store) ReplaceVersioned(ctx context.Context, key, value string, expectedRevision int64, conflict error,
	read func(context.Context) (int64, *string, error), apply func(context.Context) error,
) error {
	session, err := s.Updates().Begin(ctx)
	if err != nil {
		return err
	}
	// Keep session admission until post-commit application returns so a second
	// local writer cannot prepare its read against an unfinished replacement.
	defer session.Close()
	revision, expected, err := read(session.Context())
	if err != nil {
		return err
	}
	if revision != expectedRevision {
		return conflict
	}
	err = session.Commit(PreparedChange{
		Module: key, Values: map[string]string{key: value}, Expected: map[string]*string{key: expected}, Apply: apply,
	})
	if errors.Is(err, locale.ErrConflict) {
		return conflict
	}
	return err
}
