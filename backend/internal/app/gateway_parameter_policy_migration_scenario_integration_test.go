//go:build integration

package app

import (
	"io/fs"
	"os"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	postgresinfra "github.com/TokenFlux/TokenRouter/internal/infra/postgres"
	"github.com/TokenFlux/TokenRouter/internal/testutil/postgrescontainer"
	"github.com/TokenFlux/TokenRouter/migrations"
)

// gateway_parameter_policy_migration_scenario_integration_test.go checks the rollout migration
// with historical rows in a database first built through phase one. The
// regular migration runner applies the same embedded SQL and checksums used by
// a serving deployment.

// TestGatewayParameterPolicyMigration first builds the phase-one schema on
// an empty database, then upgrades it and preserves enrolled rows. It verifies
// each phase accepts only its configured arms and source attribution.
func TestGatewayParameterPolicyMigration(t *testing.T) {
	ctx := t.Context()
	phaseOne := fstest.MapFS{}
	files, err := fs.Glob(migrations.FS, "*.sql")
	require.NoError(t, err)
	for _, name := range files {
		if name >= "335_" {
			break
		}
		contents, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		phaseOne[name] = &fstest.MapFile{Data: contents}
	}
	db := postgrescontainer.NewOrExternalWithMigrations(t, os.Getenv("TOKENROUTER_PARAMETER_MIGRATION_TEST_DSN"), "parameter_policy_migration", phaseOne)
	insert := func(phase int, id, arm string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO gateway_compatibility_cohorts
			(phase,request_id,enrolled_at,revision,arm,group_id,provider_id,model,endpoint,client_stream)
			VALUES ($1::smallint,$2,now(),1,$3,1,CASE WHEN $1::smallint=1 THEN 1 END,'model','/v1/responses',false)`, phase, id, arm)
		return err
	}
	require.NoError(t, insert(1, "observe-one", "observe"))
	require.NoError(t, insert(1, "enforce-one", "enforce"))
	require.ErrorContains(t, insert(1, "invalid-before", "strict"), "gateway_compatibility_cohorts_arm_check")
	require.NoError(t, insert(2, "legacy-two", "enforce"))
	t.Log("empty database migrated through 334; phase-one observe and enforce admitted")

	require.NoError(t, postgresinfra.ApplyMigrations(ctx, db, migrations.FS))
	require.NoError(t, postgresinfra.ApplyMigrations(ctx, db, migrations.FS))
	var firstArm, legacyArm string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT arm FROM gateway_compatibility_cohorts WHERE request_id='enforce-one'`).Scan(&firstArm))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT arm FROM gateway_compatibility_cohorts WHERE request_id='legacy-two'`).Scan(&legacyArm))
	require.Equal(t, "enforce", firstArm)
	require.Equal(t, "strict", legacyArm)
	require.NoError(t, insert(2, "observe-two", "observe"))
	require.NoError(t, insert(2, "strict-two", "strict"))
	require.ErrorContains(t, insert(1, "wrong-one", "strict"), "gateway_compatibility_cohorts_arm_check")
	require.ErrorContains(t, insert(2, "wrong-two", "enforce"), "gateway_compatibility_cohorts_arm_check")
	_, err = db.ExecContext(ctx, `UPDATE gateway_compatibility_cohorts SET outcome='parameter_400',
		finished_at=now(), duration_ms=0, rejection_source='entry' WHERE request_id='strict-two'`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE gateway_compatibility_cohorts SET outcome='parameter_400',
		finished_at=now(), duration_ms=0, final_revision=1, rejection_source='entry' WHERE request_id='observe-one'`)
	require.ErrorContains(t, err, "gateway_compatibility_cohorts_rejection_source_check")
	t.Log("335 upgraded historical rows; cross-phase arms and phase-one rejection source rejected")
}
