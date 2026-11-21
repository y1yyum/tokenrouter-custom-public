package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	postgresinfra "github.com/TokenFlux/TokenRouter/internal/infra/postgres"
	"github.com/TokenFlux/TokenRouter/internal/provider"
)

// plan_model_verdict.go keeps a quality decision, its watermark, and the
// scheduler event in one statement. The result ledger survives plan-result
// retention. Selection can read the active owner rows without changing the
// administrator's provider health flags or acquiring execution capacity.

// ApplyPlanModelVerdict admits at most one grade per persisted, successful
// probe exchange. Concurrent grades for one owner advance both the policy
// version and the start watermark. Replays and stale grades produce no event.
func (r *ProviderStore) ApplyPlanModelVerdict(ctx context.Context, verdict provider.PlanModelVerdict) (bool, error) {
	if r.options.Events == nil {
		return false, errors.New("plan model verdict requires scheduler events")
	}
	return r.applyPlanModelVerdict(ctx, r.sql, verdict)
}

// applyPlanModelVerdict uses the caller's connection so a quality result,
// its owner watermark, and its scheduling event can share one transaction.
func (r *ProviderStore) applyPlanModelVerdict(ctx context.Context, db postgresinfra.Executor, verdict provider.PlanModelVerdict) (bool, error) {
	result, err := db.ExecContext(ctx, `
		WITH observed AS (
			INSERT INTO provider_plan_model_verdicts
				(result_id, provider_id, plan_id, policy_version, requested_model,
				 upstream_model, started_at, restricted, reason, expires_at)
			SELECT results.id, plans.provider_id, plans.id, $4, $5, $6,
			       results.started_at, $7, $8, $9
			FROM scheduled_test_results AS results
			JOIN scheduled_test_plans AS plans ON plans.id = results.plan_id
			WHERE results.id = $1 AND plans.id = $2 AND plans.provider_id = $3
			  AND plans.enabled AND plans.model_id = $5 AND results.status = 'success'
			ON CONFLICT (result_id) DO NOTHING
			RETURNING *
		), applied AS (
			INSERT INTO provider_plan_model_state AS state
				(provider_id, plan_id, upstream_model, result_id, policy_version, started_at)
			SELECT provider_id, plan_id, upstream_model, result_id, policy_version, started_at FROM observed
			ON CONFLICT (provider_id, upstream_model, plan_id) DO UPDATE
			SET result_id = EXCLUDED.result_id, policy_version = EXCLUDED.policy_version,
			    started_at = EXCLUDED.started_at
			WHERE EXCLUDED.policy_version >= state.policy_version AND
			      (EXCLUDED.started_at, EXCLUDED.result_id) > (state.started_at, state.result_id)
			RETURNING provider_id
		)
		INSERT INTO scheduler_outbox (event_type, provider_id)
		SELECT $10, provider_id FROM applied`,
		verdict.ResultID, verdict.PlanID, verdict.ProviderID, verdict.PolicyVersion, verdict.RequestedModel,
		verdict.UpstreamModel, verdict.Restricted, verdict.Reason, verdict.ExpiresAt, r.eventName(ProviderChanged))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

// ActivePlanModelRestrictions returns every unexpired restriction in stable
// plan order. An expired or passing latest grade leaves the earlier grade
// available for audit while keeping it out of scheduling decisions.
func (r *ProviderStore) ActivePlanModelRestrictions(ctx context.Context, providerID int64, upstreamModel string, at time.Time) ([]provider.ActivePlanModelRestriction, error) {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT v.result_id, v.plan_id, v.provider_id, v.policy_version, v.requested_model,
		       v.upstream_model, v.restricted, v.reason, v.expires_at, v.started_at
		FROM provider_plan_model_state s
		JOIN provider_plan_model_verdicts v ON v.result_id = s.result_id
		JOIN scheduled_test_plans p ON p.id = s.plan_id AND p.provider_id = s.provider_id
		WHERE s.provider_id = $1 AND s.upstream_model = $2 AND v.restricted
		  AND (v.expires_at IS NULL OR v.expires_at > $3)
		ORDER BY s.plan_id`, providerID, upstreamModel, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var owners []provider.ActivePlanModelRestriction
	for rows.Next() {
		var v provider.ActivePlanModelRestriction
		if err := rows.Scan(&v.ResultID, &v.PlanID, &v.ProviderID, &v.PolicyVersion, &v.RequestedModel,
			&v.UpstreamModel, &v.Restricted, &v.Reason, &v.ExpiresAt, &v.StartedAt); err != nil {
			return nil, err
		}
		owners = append(owners, v)
	}
	return owners, rows.Err()
}

// SaveQualityResult binds a grade to its generated result ID before retention.
// A plan edit is serialized against this transaction. Disabled and stale probes
// keep a diagnostic history row without advancing the owner's watermark.
func (r *ProviderStore) SaveQualityResult(ctx context.Context, db *sql.DB, result *provider.ScheduledTestResult) (*provider.ScheduledTestResult, error) {
	if result.Quality == nil || r.options.Events == nil {
		return nil, errors.New("quality observations require a grade and scheduler events")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var current bool
	q := *result.Quality
	err = tx.QueryRowContext(ctx, `SELECT enabled AND quality_enabled AND quality_revision=$2 AND provider_id=$3 AND model_id=$4
   FROM scheduled_test_plans WHERE id=$1 FOR SHARE`, result.PlanID, q.Revision, q.ProviderID, q.RequestedModel).Scan(&current)
	if err != nil {
		return nil, err
	}
	if !current {
		q.Grade = "ungraded"
		q.Reason = "plan_changed"
	}
	observation := *result
	observation.Quality = &q
	stored, err := insertScheduledResult(ctx, tx, &observation)
	if err != nil {
		return nil, err
	}
	if q.Grade == "pass" || q.Grade == "fail" {
		until := stored.StartedAt.Add(provider.QualityRestrictionTTL)
		_, err = r.applyPlanModelVerdict(ctx, tx, provider.PlanModelVerdict{
			ResultID: stored.ID, PlanID: stored.PlanID, ProviderID: q.ProviderID,
			PolicyVersion: q.Version, RequestedModel: q.RequestedModel, UpstreamModel: q.UpstreamModel, Restricted: q.Grade == "fail",
			Reason: q.Rule + ": " + q.Reason, ExpiresAt: &until,
		})
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stored, nil
}

// ModelQuality reads all counters and watermarks in one PostgreSQL snapshot.
// Only identified user traffic contributes. Recovered errors have a successful
// client status. Duplicate terminal errors for the same key and request count
// once, and a recorded success takes precedence over a matching error.
func (r *ProviderStore) ModelQuality(ctx context.Context, id int64, model string, at time.Time) (*provider.ModelQualityReport, error) {
	report := &provider.ModelQualityReport{From: at.Add(-24 * time.Hour), Until: at, ProviderID: id, Model: model}
	var owners []byte
	rows, err := r.sql.QueryContext(ctx, `
		WITH successes AS (
			SELECT api_key_id, request_id, cache_read_tokens,
			       input_tokens::bigint + cache_creation_tokens + cache_read_tokens AS total_input
			FROM usage_logs WHERE provider_id=$1 AND COALESCE(NULLIF(upstream_model,''),model)=$2
			  AND request_source='user' AND request_type <> 4 AND created_at >= $3 AND created_at < $4
		), failures AS (
			SELECT DISTINCT e.api_key_id, e.request_id FROM ops_error_logs e
			WHERE e.provider_id=$1 AND e.upstream_model=$2 AND e.created_at >= $3 AND e.created_at < $4
			  AND e.api_key_id IS NOT NULL AND NULLIF(e.request_id,'') IS NOT NULL
			  AND e.status_code >= 400 AND e.error_owner='provider' AND NOT e.is_business_limited
			  AND NOT EXISTS (SELECT 1 FROM successes s WHERE s.api_key_id=e.api_key_id AND s.request_id=e.request_id)
		), grades AS (
			SELECT restricted FROM provider_plan_model_verdicts WHERE provider_id=$1 AND upstream_model=$2
			  AND started_at >= $3 AND started_at < $4
		), owners AS (
			SELECT v.plan_id, v.result_id, v.requested_model, v.reason, v.started_at, v.expires_at,
			       CASE WHEN NOT v.restricted THEN 'passed' WHEN v.expires_at <= $4 THEN 'expired' ELSE 'restricted' END AS state
			FROM provider_plan_model_state s JOIN provider_plan_model_verdicts v USING(result_id)
			JOIN scheduled_test_plans p ON p.id=s.plan_id AND p.provider_id=s.provider_id
			WHERE s.provider_id=$1 AND s.upstream_model=$2
		)
		SELECT count(*), (SELECT count(*) FROM failures), count(*) FILTER (WHERE total_input>0),
		       count(*) FILTER (WHERE total_input>0 AND cache_read_tokens>0), COALESCE(sum(total_input) FILTER (WHERE total_input>0),0),
		       COALESCE(sum(cache_read_tokens) FILTER (WHERE total_input>0),0),
		       (SELECT count(*) FROM grades), (SELECT count(*) FROM grades WHERE restricted),
		       (SELECT COALESCE(json_agg(owners ORDER BY plan_id),'[]') FROM owners)
		FROM successes`, id, model, report.From, report.Until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	err = rows.Scan(&report.Success, &report.Failed, &report.CacheSamples,
		&report.CacheHits, &report.InputTokens, &report.ReadTokens, &report.Graded, &report.Wrong, &owners)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(owners, &report.Owners); err != nil {
		return nil, err
	}
	return report, nil
}
