package cryptopay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// config_cas.go binds configuration edits to the stored snapshot they operate on.
// Runtime settings store the entire configuration as one JSON value. Two callers
// may read the same document and make independent changes, but accepting both
// writes unconditionally can discard the earlier change or overwrite the
// sweep destination that an operator validated.
//
// The reader decodes normalized settings for the patch while retaining the
// original text to match during persistence. A missing row has a separate
// expectation so even evidence of an empty row created concurrently rejects
// the first-write attempt.

var (
	// ErrConfigConflict reports that another writer changed the raw snapshot
	// while the caller was preparing an edit.
	ErrConfigConflict = apperror.Conflict("CRYPTO_CONFIG_CONFLICT", "充值配置已被其他请求修改，请刷新后重新验证并保存")

	// errAtomicSettingUnavailable indicates that the store cannot honor the
	// snapshot contract for configuration writes.
	errAtomicSettingUnavailable = apperror.ServiceUnavailable("ATOMIC_SETTING_UNAVAILABLE", "Atomic settings updates are unavailable")
)

// AtomicSettingStore provides uncached snapshots and conditional writes for configuration patches.
type AtomicSettingStore interface {
	// GetCryptoSettingUncached reads the currently persisted text for a key.
	// Missing rows return settings.ErrSettingNotFound, and the successful result
	// must be suitable for direct comparison by the writer.
	GetCryptoSettingUncached(context.Context, string) (string, error)
	// CompareAndSwapCryptoSetting writes only when the saved row matches the
	// expected text, or when the key is absent for a nil expectation. It returns
	// false without an error for a mismatch.
	CompareAndSwapCryptoSetting(context.Context, string, *string, string) (bool, error)
}

// LoadConfigForUpdate reads the snapshot used to prepare a conditional edit.
// The decoded configuration overlays the stored JSON onto the current defaults,
// while expected retains the original text with its whitespace and JSON
// formatting. A nil expectation represents an absent key and still returns
// the first-run defaults. Callers pass expected through to SaveConfigIfUnchanged
// so the database compares against the row used to prepare the edit.
func LoadConfigForUpdate(ctx context.Context, store SettingStore) (cfg CryptoConfig, expected *string, err error) {
	atomicStore, ok := store.(AtomicSettingStore)
	if !ok {
		return cfg, nil, errAtomicSettingUnavailable
	}
	raw, err := atomicStore.GetCryptoSettingUncached(ctx, SettingKeyCryptoConfig)
	if errors.Is(err, settings.ErrSettingNotFound) {
		// Reserve nil for an absent row so a concurrent first save cannot be
		// mistaken for a saved blank document.
		return DefaultCryptoConfig(), nil, nil
	}
	if err != nil {
		return cfg, nil, err
	}
	cfg = DefaultCryptoConfig()
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return cfg, nil, fmt.Errorf("crypto config: invalid JSON: %w", err)
		}
	}
	cfg, err = NormalizeConfig(cfg)
	return cfg, &raw, err
}

// SaveConfigIfUnchanged validates and saves an edit only against its original
// snapshot. The caller supplies expected from LoadConfigForUpdate. Read-only
// instances reject the write, and a stale snapshot returns ErrConfigConflict.
// Interactive destination changes require fresh user action after a conflict so
// the next save still applies to the destination that was approved and notified.
func SaveConfigIfUnchanged(ctx context.Context, store SettingStore, cfg CryptoConfig, expected *string) error {
	if err := mode.CheckWritable(); err != nil {
		return err
	}
	atomicStore, ok := store.(AtomicSettingStore)
	if !ok {
		return errAtomicSettingUnavailable
	}
	normalized, err := NormalizeConfig(cfg)
	if err != nil {
		return err
	}
	data, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	changed, err := atomicStore.CompareAndSwapCryptoSetting(ctx, SettingKeyCryptoConfig, expected, string(data))
	if err != nil {
		return err
	}
	if !changed {
		return ErrConfigConflict
	}
	return nil
}
