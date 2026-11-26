package postgres

import (
	"context"
	"database/sql"
)

// bark_settings.go persists encrypted Bark preferences on the owning user row.
// A settings change must notify the original device before the replacement is
// committed. The transaction holds its row lock through that callback so a
// concurrent edit cannot redirect the notification.

// BarkSettingsStore locks the recipient row while the service notifies the original device.
type BarkSettingsStore struct{ db *sql.DB }

// NewBarkSettingsStore binds notification settings to the application's database.
func NewBarkSettingsStore(db *sql.DB) *BarkSettingsStore { return &BarkSettingsStore{db: db} }

// Read returns encrypted settings. Security alerts require a currently active
// administrator, while ordinary account notifications use the unfiltered row.
func (s *BarkSettingsStore) Read(ctx context.Context, userID int64, activeAdminOnly bool) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, sql.ErrConnDone
	}
	query := "SELECT bark_device_key_encrypted, bark_notify_enabled FROM users WHERE id=$1"
	if activeAdminOnly {
		query += " AND role='admin' AND status='active' AND deleted_at IS NULL"
	}
	var encrypted string
	var enabled bool
	err := s.db.QueryRowContext(ctx, query, userID).Scan(&encrypted, &enabled)
	return encrypted, enabled, err
}

// Update locks an active account, applies its callback, and commits both
// preferences together. No-op callbacks and failures leave the original row intact.
func (s *BarkSettingsStore) Update(ctx context.Context, userID int64, change func(string, bool) (string, bool, error)) error {
	if s == nil || s.db == nil {
		return sql.ErrConnDone
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var encrypted string
	var enabled bool
	err = tx.QueryRowContext(ctx, "SELECT bark_device_key_encrypted,bark_notify_enabled FROM users WHERE id=$1 AND status='active' AND deleted_at IS NULL FOR UPDATE", userID).Scan(&encrypted, &enabled)
	if err != nil {
		return err
	}
	next, on, err := change(encrypted, enabled)
	if err != nil || (next == encrypted && on == enabled) {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET bark_device_key_encrypted=$1,bark_notify_enabled=$2 WHERE id=$3", next, on, userID); err != nil {
		return err
	}
	return tx.Commit()
}
