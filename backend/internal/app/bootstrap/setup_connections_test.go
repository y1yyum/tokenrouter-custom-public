package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Database creation requires a maintenance connection before the target
// exists. This fixture gives setup a distinct target name and checks the two
// DSNs, keeping maintenance access on postgres and the later verification
// connection on the requested database.

// TestBuildDatabaseConnectionDSNs checks setup with and without a database
// password. The maintenance and target DSNs must name their respective
// databases even when the password keyword is omitted.
func TestBuildDatabaseConnectionDSNs(t *testing.T) {
	tests := []struct {
		name            string
		password        string
		wantMaintenance string
		wantTarget      string
	}{
		{
			name:            "empty password",
			wantMaintenance: "host=db port=5433 user=crypto_review dbname=postgres sslmode=disable",
			wantTarget:      "host=db port=5433 user=crypto_review dbname=crypto_http sslmode=disable",
		},
		{
			name:            "configured password",
			password:        "example-pass",
			wantMaintenance: "host=db port=5433 user=crypto_review password=example-pass dbname=postgres sslmode=disable",
			wantTarget:      "host=db port=5433 user=crypto_review password=example-pass dbname=crypto_http sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &SetupDatabaseConfig{
				Host:     "db",
				Port:     5433,
				User:     "crypto_review",
				Password: tt.password,
				DBName:   "crypto_http",
				SSLMode:  "disable",
			}

			maintenanceDSN, targetDSN := BuildDatabaseConnectionDSNs(cfg)

			require.Equal(t, tt.wantMaintenance, maintenanceDSN, "maintenance DSN")
			require.Equal(t, tt.wantTarget, targetDSN, "target DSN")
		})
	}
}
