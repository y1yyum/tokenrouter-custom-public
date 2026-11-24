package cryptopay

import (
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/TokenFlux/TokenRouter/internal/config"
)

// TestWalletUsesPersistentInstallationKeyWithoutDedicatedPaymentKey gives the
// service identical key material under both host configuration states. Wallet
// encryption must be available only when the host marks the key as persistent.
func TestWalletUsesPersistentInstallationKeyWithoutDedicatedPaymentKey(t *testing.T) {
	t.Setenv("SUB2API_DISABLE_BACKGROUND_JOBS", "true")
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "process-local", true: "persisted"}[persistent], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			settings := newConfigTestStore(readOnlyConfigRepository{raw: "{}"})
			appConfig := &config.Config{Security: config.SecurityConfig{
				SecretEncryptionKey: strings.Repeat("ab", 32), SecretEncryptionKeyConfigured: persistent,
			}}
			s := NewService(db, settings, nil, nil, appConfig)
			mock.ExpectQuery("SELECT value FROM settings").WithArgs("crypto.signer.vault").
				WillReturnRows(sqlmock.NewRows([]string{"value"}))
			status, err := s.WalletStatus(t.Context())
			if err != nil || status.OK != persistent {
				t.Fatalf("wallet encryption ready = %v, want %v, err=%v", status, persistent, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
