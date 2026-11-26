package postgres

import (
	"context"
	"database/sql"

	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
)

// compatibility_cohorts.go writes rollout enrollment and the final HTTP
// classification to the gateway database. Request identity is local to this
// site. A request evaluated by both policies has one row for each phase;
// retry attempts share their original row and never change its rule or arm.

// CompatibilityCohorts holds the shared application connection pool.
type CompatibilityCohorts struct {
	db *sql.DB // database lifecycle belongs to application assembly
}

// NewCompatibilityCohorts connects rollout monitoring to the primary database.
func NewCompatibilityCohorts(db *sql.DB) *CompatibilityCohorts {
	return &CompatibilityCohorts{db: db}
}

// Enroll records an in-flight logical request. An existing row already owns
// the first candidate and cohort, including if its finish was observed first.
func (s *CompatibilityCohorts) Enroll(ctx context.Context, c responsecompat.Cohort) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO gateway_compatibility_cohorts
		(phase,request_id,enrolled_at,revision,arm,group_id,provider_id,model,endpoint,client_stream)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7::bigint,0),$8,$9,$10)
		ON CONFLICT (phase,request_id) DO NOTHING`, c.Phase, c.RequestID, c.EnrolledAt, c.Revision,
		c.Arm, c.Tuple.EffectiveGroupID, c.Tuple.ProviderID, c.Tuple.UpstreamModel, c.Tuple.UpstreamEndpoint, c.ClientStream)
	return err
}

// Finish publishes the final classification once. The insert also recovers
// an enrollment lost to a temporary write failure. Conflict updates preserve
// the original arm and never replace a finalized classification or revision.
// The final evaluation revision belongs only to the Responses phase.
func (s *CompatibilityCohorts) Finish(ctx context.Context, c responsecompat.Cohort, result responsecompat.CohortResult) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO gateway_compatibility_cohorts
		(phase,request_id,enrolled_at,revision,arm,group_id,provider_id,model,endpoint,client_stream,
		 applied,outcome,target_parameter_error,finished_at,duration_ms,rejection_source,final_revision)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7::bigint,0),$8,$9,$10,$11,$12,$13,$14,$15,NULLIF($16::text,''),
		 CASE WHEN $1::smallint = 1 THEN $17::bigint END)
		ON CONFLICT (phase,request_id) DO UPDATE SET
		  applied=EXCLUDED.applied, outcome=EXCLUDED.outcome, target_parameter_error=EXCLUDED.target_parameter_error,
		  finished_at=EXCLUDED.finished_at, duration_ms=EXCLUDED.duration_ms,
		  rejection_source=EXCLUDED.rejection_source, final_revision=EXCLUDED.final_revision
		WHERE gateway_compatibility_cohorts.outcome IS NULL`, c.Phase, c.RequestID, c.EnrolledAt, c.Revision,
		c.Arm, c.Tuple.EffectiveGroupID, c.Tuple.ProviderID, c.Tuple.UpstreamModel, c.Tuple.UpstreamEndpoint,
		c.ClientStream, result.Applied, result.Outcome, result.TargetParameterError, result.FinishedAt,
		result.Duration.Milliseconds(), result.RejectionSource, result.FinalRevision)
	return err
}
