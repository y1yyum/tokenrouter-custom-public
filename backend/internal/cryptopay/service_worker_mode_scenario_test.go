package cryptopay

import (
	"testing"
)

// service_worker_mode_scenario_test.go exercises service.go and scan_job.go.

func TestCryptoWorkersRespectIsolatedAndReadOnlyModes(t *testing.T) {
	t.Setenv("SUB2API_DISABLE_BACKGROUND_JOBS", "")
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "")
	if !cryptoWorkersAllowed() {
		t.Fatal("normal instances must retain crypto scanning and scheduled maintenance")
	}

	t.Setenv("SUB2API_DISABLE_BACKGROUND_JOBS", "true")
	if cryptoWorkersAllowed() {
		t.Fatal("background-job-disabled instances must not run crypto workers")
	}

	t.Setenv("SUB2API_DISABLE_BACKGROUND_JOBS", "")
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "true")
	if cryptoWorkersAllowed() {
		t.Fatal("read-only test instances must not scan deposits or run automatic sweeps")
	}
}
