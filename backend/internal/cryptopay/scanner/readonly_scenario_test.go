package scanner

import (
	"errors"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
)

// readonly_scenario_test.go exercises worker.go and cursor.go.

func TestAdminTestModeDisablesScannerAndRecoveryBeforeDatabaseAccess(t *testing.T) {
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "true")
	w := NewWorker(nil, nil, []ChainConfig{{Chain: "bsc"}}, nil, nil, nil, 0, 0)
	w.Start()
	if w.cancel != nil {
		w.Stop()
		t.Fatal("read-only instance started the scanner")
	}
	w.scanAll(t.Context()) // must not even attempt its PostgreSQL lock
	for _, call := range []func() error{
		func() error { return w.ScanOnce(t.Context()) },
		func() error { return w.retryPending(t.Context()) },
		func() error { return w.recheckCredited(t.Context()) },
		func() error { return NewCursorStore(nil).Save(t.Context(), "bsc", 42) },
	} {
		if err := call(); !errors.Is(err, mode.ErrReadOnly) {
			t.Fatalf("scanner attempted shared database access: %v", err)
		}
	}
}
