package notification

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/notification/bark"
)

// DeviceKeyEncryptor encrypts/decrypts user-supplied Bark keys at rest.
// Implemented by the repository-layer AES encryptor.
type DeviceKeyEncryptor interface {
	EncryptString(plaintext string) (string, error)
	DecryptString(ciphertext string) (string, error)
}

// encryptorAdapter adapts the repository AESEncryptor (Encrypt/Decrypt) to
// the interface above without leaking that package into callers.
type encryptorAdapter struct {
	inner interface {
		Encrypt(plaintext string) (string, error)
		Decrypt(ciphertext string) (string, error)
	}
}

// AdaptEncryptor wraps any Encrypt/Decrypt pair for the Bark service.
func AdaptEncryptor(inner interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}) DeviceKeyEncryptor {
	return &encryptorAdapter{inner: inner}
}

func (a *encryptorAdapter) EncryptString(plaintext string) (string, error) {
	return a.inner.Encrypt(plaintext)
}

func (a *encryptorAdapter) DecryptString(ciphertext string) (string, error) {
	return a.inner.Decrypt(ciphertext)
}

// UserBarkService reads per-user Bark settings and sends pushes.
// The encrypted device key never leaves this layer in plaintext form.
type UserBarkService struct {
	db        *sql.DB
	encryptor DeviceKeyEncryptor
	newSender func(string) bark.Sender
}

func NewUserBarkService(db *sql.DB, encryptor DeviceKeyEncryptor) *UserBarkService {
	return &UserBarkService{db: db, encryptor: encryptor, newSender: func(key string) bark.Sender { return bark.New(key) }}
}

var (
	ErrBarkKeyRequired   = errors.New("configure a Bark device key before enabling notifications")
	ErrBarkInvalidKey    = errors.New("invalid Bark device key")
	ErrBarkNotConfigured = errors.New("wallet owner Bark notifications are not configured or enabled")
)

// SendToUserRequired is for security alerts. A missing recipient is an error,
// so callers keep the durable alert pending instead of silently acknowledging it.
func (s *UserBarkService) SendToUserRequired(ctx context.Context, userID int64, n bark.Notification) error {
	if s == nil || s.db == nil || s.encryptor == nil || s.newSender == nil {
		return ErrBarkNotConfigured
	}
	var encrypted string
	var enabled bool
	err := s.db.QueryRowContext(ctx, `SELECT bark_device_key_encrypted, bark_notify_enabled FROM users
		WHERE id = $1 AND role = 'admin' AND status = 'active' AND deleted_at IS NULL`, userID).Scan(&encrypted, &enabled)
	if err != nil {
		return ErrBarkNotConfigured
	}
	if !enabled || strings.TrimSpace(encrypted) == "" {
		return ErrBarkNotConfigured
	}
	key, err := s.encryptor.DecryptString(encrypted)
	if err != nil || strings.TrimSpace(key) == "" {
		return errors.New("wallet owner Bark device key unavailable")
	}
	sender := s.newSender(key)
	if sender == nil {
		return ErrBarkNotConfigured
	}
	return sender.Send(ctx, n)
}

// Status reports whether the user has a key configured (never the key).
func (s *UserBarkService) Status(ctx context.Context, userID int64) (configured, enabled bool, err error) {
	var encrypted string
	err = s.db.QueryRowContext(ctx,
		`SELECT bark_device_key_encrypted, bark_notify_enabled FROM users WHERE id = $1`,
		userID).Scan(&encrypted, &enabled)
	if err != nil {
		return false, false, fmt.Errorf("read bark settings: %w", err)
	}
	return strings.TrimSpace(encrypted) != "", enabled, nil
}

// SendToUser delivers a push if the user has Bark enabled.
func (s *UserBarkService) SendToUser(ctx context.Context, userID int64, n bark.Notification) error {
	var encrypted string
	var enabled bool
	err := s.db.QueryRowContext(ctx,
		`SELECT bark_device_key_encrypted, bark_notify_enabled FROM users WHERE id = $1`,
		userID).Scan(&encrypted, &enabled)
	if err != nil {
		return fmt.Errorf("read bark settings: %w", err)
	}
	if !enabled || strings.TrimSpace(encrypted) == "" {
		return nil
	}
	key, err := s.encryptor.DecryptString(encrypted)
	if err != nil {
		return fmt.Errorf("decrypt bark key: %w", err)
	}
	return bark.New(key).Send(ctx, n)
}
