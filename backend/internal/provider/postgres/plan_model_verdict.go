package postgres

import (
	"context"
	"errors"
	"time"

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
	result, err := r.sql.ExecContext(ctx, `
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
