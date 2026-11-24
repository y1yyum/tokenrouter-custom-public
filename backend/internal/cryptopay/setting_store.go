package cryptopay

import (
	"context"
	"database/sql"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/addresses"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// setting_store.go connects crypto configuration to the host settings table.
// The host store reads and writes persisted text. A single conditional SQL
// statement handles a configuration patch because the host's batch API treats
// a missing key and a saved empty value alike. The distinction matters when a
// patch prepared against a missing row races with another writer creating it.
//
// Both write paths check the process-local read-only gate before persistence.
// Reads remain available to review instances, and conditional writes compare
// the raw text captured by the caller, including whitespace.

var (
	// Verify the host adapter keeps the read and conditional-write contracts
	// needed by callers that prepare configuration edits.
	_ AtomicSettingStore = (*ServiceSettingStore)(nil)
)

// ServiceSettingStore holds the host dependencies for raw crypto settings.
type ServiceSettingStore struct {
	store *settings.Store // host raw settings store
	db    *sql.DB         // same PostgreSQL settings database
}

// NewAddressService builds the address persistence service using the host
// database. The caller provides the wallet and its security guards through the
// crypto facade before using the address service.
func NewAddressService(db *sql.DB) *addresses.Service {
	return addresses.NewService(addresses.NewRepo(db), nil)
}

// NewServiceSettingStore binds the shared settings store to its PostgreSQL
// database. Both dependencies must address the same settings table so snapshots
// can be used for conditional writes.
func NewServiceSettingStore(store *settings.Store, db *sql.DB) *ServiceSettingStore {
	return &ServiceSettingStore{store: store, db: db}
}

// GetCryptoSetting reads the persisted, unmodified text using the host store.
// A saved empty string succeeds, and a missing key returns settings.ErrSettingNotFound.
func (s *ServiceSettingStore) GetCryptoSetting(ctx context.Context, key string) (string, error) {
	if s == nil || s.store == nil {
		return "", errAtomicSettingUnavailable
	}
	return s.store.GetValue(ctx, key)
}

// SetCryptoSetting saves the supplied value through the host settings store.
// Read-only instances reject the write before storage access. Use the conditional
// writer when replacing a previously observed value.
func (s *ServiceSettingStore) SetCryptoSetting(ctx context.Context, key, value string) error {
	if err := mode.CheckWritable(); err != nil {
		return err
	}
	if s == nil || s.store == nil {
		return errAtomicSettingUnavailable
	}
	if err := s.store.Set(ctx, key, value); err != nil {
		return err
	}
	if key == SettingKeyCryptoConfig {
		s.notifyPublicSettings()
	}
	return nil
}

// notifyPublicSettings invalidates the host's cached bootstrap HTML after a
// persisted channel change. The public settings reader projects the enabled
// flag itself, so subscribers never receive the private configuration document.
func (s *ServiceSettingStore) notifyPublicSettings() {
	if s != nil && s.store != nil {
		s.store.NotifyUpdated()
	}
}

// GetCryptoSettingUncached reads a fresh snapshot from the host settings store.
// The production repository queries PostgreSQL on each call, returning the raw
// text the caller needs for a concurrent edit check.
func (s *ServiceSettingStore) GetCryptoSettingUncached(ctx context.Context, key string) (string, error) {
	return s.GetCryptoSetting(ctx, key)
}

// CompareAndSwapCryptoSetting replaces the value only if the key still matches
// expected and reports whether a row changed. A nil expected value requests an
// insert for an absent key, while a pointer to an empty string requires a saved
// empty value. A mismatch returns false without an error.
//
// Each path checks and writes in one database statement so a concurrent process
// can neither win a second insert nor overwrite a newer configuration using a
// stale snapshot. Read-only instances reject both paths before storage access.
func (s *ServiceSettingStore) CompareAndSwapCryptoSetting(ctx context.Context, key string, expected *string, value string) (bool, error) {
	if err := mode.CheckWritable(); err != nil {
		return false, err
	}
	if s == nil || s.db == nil {
		return false, errAtomicSettingUnavailable
	}

	var result sql.Result
	var err error
	if expected == nil {
		// Use the unique key to resolve first-write races, including when the
		// winning request inserts an empty value.
		result, err = s.db.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (key) DO NOTHING
		`, key, value)
	} else {
		// Compare persisted text in a deterministic collation, preserving
		// whitespace and byte differences independently of database locale.
		result, err = s.db.ExecContext(ctx, `
			UPDATE settings
			SET value = $2, updated_at = NOW()
			WHERE key = $1 AND value COLLATE "C" = $3
		`, key, value, *expected)
	}
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	// Publish only a committed configuration change. A stale compare-and-swap
	// leaves both the persisted configuration and the cached browser settings intact.
	if err == nil && count == 1 && key == SettingKeyCryptoConfig {
		s.notifyPublicSettings()
	}
	return count == 1, err
}
