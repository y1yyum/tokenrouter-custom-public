//go:build integration

package app

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	gatewaypg "github.com/TokenFlux/TokenRouter/internal/gateway/postgres"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
)

// compatibility_cohorts_scenario_integration_test.go checks the persisted denominator
// across retries, late admission delivery and two independently evaluated
// policies. It uses the migrated database so primary-key and nullability
// constraints participate in every recorded transition.

// TestCompatibilityCohortLifecycle compares the stored arm and outcome after
// duplicate admissions and conflicting finishes. A second phase can finish
// even when its initial write was missed, without changing the first phase.
func TestCompatibilityCohortLifecycle(t *testing.T) {
	db := qualityTestDatabase(t)
	repo := gatewaypg.NewCompatibilityCohorts(db)
	ctx := t.Context()
	cohort := responsecompat.Cohort{
		Phase: responsecompat.ResponsesPhase, RequestID: "logical-1", EnrolledAt: time.Now(), Revision: 1,
		Arm: responsecompat.Enforce, Tuple: responsecompat.Tuple{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"},
	}
	require.NoError(t, repo.Enroll(ctx, cohort))
	changed := cohort
	changed.Arm, changed.Revision, changed.Tuple.ProviderID = responsecompat.Observe, 2, 58
	require.NoError(t, repo.Enroll(ctx, changed))
	var pending int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM gateway_compatibility_cohorts WHERE outcome IS NULL AND final_revision IS NULL").Scan(&pending))
	require.Equal(t, 1, pending)
	result := responsecompat.CohortResult{Outcome: "incomplete", Applied: true, FinishedAt: time.Now(), Duration: time.Second, FinalRevision: 2}
	require.NoError(t, repo.Finish(ctx, changed, result))
	result.Outcome = "other_failure"
	result.FinalRevision = 3
	require.NoError(t, repo.Finish(ctx, changed, result))
	var outcome, arm string
	var revision, finalRevision, provider int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT outcome,arm,revision,final_revision,provider_id FROM gateway_compatibility_cohorts WHERE phase=1").Scan(&outcome, &arm, &revision, &finalRevision, &provider))
	require.Equal(t, "incomplete", outcome)
	require.Equal(t, "enforce", arm)
	require.EqualValues(t, 1, revision)
	require.EqualValues(t, 2, finalRevision)
	require.EqualValues(t, 57, provider)
	cohort.Phase, cohort.Tuple.ProviderID = responsecompat.ParameterPhase, 0
	cohort.Arm = "strict"
	result.Outcome, result.RejectionSource, result.FinalRevision = "parameter_400", responsecompat.RejectionEntry, 0
	require.NoError(t, repo.Finish(ctx, cohort, result))
	require.NoError(t, repo.Enroll(ctx, cohort))
	var parameterRevision sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT final_revision FROM gateway_compatibility_cohorts WHERE phase=2").Scan(&parameterRevision))
	require.False(t, parameterRevision.Valid)
	var total, finished int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*),count(outcome) FROM gateway_compatibility_cohorts").Scan(&total, &finished))
	require.Equal(t, 2, total)
	require.Equal(t, total, finished)
}
