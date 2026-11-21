//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/ent"
	_ "github.com/TokenFlux/TokenRouter/ent/runtime"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/testutil/postgrescontainer"
)

// planVerdictEvents names the provider invalidation written by the atomic SQL
// statement. Other callbacks are deliberately absent from this storage fixture.
type planVerdictEvents struct{ ProviderEvents }

// Name returns the scheduler's complete-provider invalidation event name.
func (planVerdictEvents) Name(ProviderEvent) string { return "provider_changed" }

// TestPlanModelVerdicts exercises two owners on a manually disabled provider.
// Independent commits contend on one plan's watermark, then retained audit and
// active owners are read after the original result rows have been pruned.
func TestPlanModelVerdicts(t *testing.T) {
	db := postgrescontainer.New(t)
	ctx := context.Background()
	client := ent.NewClient(ent.Driver(sql.OpenDB(dialect.Postgres, db)))
	p, err := client.Provider.Create().SetName("restricted").SetPlatform("openai").SetType("apikey").
		SetCredentials(map[string]any{}).SetSchedulable(false).Save(ctx)
	require.NoError(t, err)
	store := NewProviderStore(client, db, ProviderStoreOptions{Events: planVerdictEvents{}})
	now := time.Now().UTC().Truncate(time.Second)
	newPlan := func() int64 {
		var id int64
		require.NoError(t, db.QueryRowContext(ctx,
			"INSERT INTO scheduled_test_plans(provider_id, model_id) VALUES ($1,'client-x') RETURNING id", p.ID).Scan(&id))
		return id
	}
	grade := func(plan, version int64, offset time.Duration, status string, restricted bool) provider.PlanModelVerdict {
		start := now.Add(offset)
		var resultID int64
		require.NoError(t, db.QueryRowContext(ctx,
			"INSERT INTO scheduled_test_results(plan_id, status, started_at, finished_at) VALUES ($1,$2,$3,$3) RETURNING id",
			plan, status, start).Scan(&resultID))
		return provider.PlanModelVerdict{ResultID: resultID, PlanID: plan, ProviderID: p.ID, PolicyVersion: version,
			RequestedModel: "client-x", UpstreamModel: "X", Restricted: restricted, Reason: "fixture grade"}
	}
	apply := func(v provider.PlanModelVerdict, want bool) {
		applied, err := store.ApplyPlanModelVerdict(ctx, v)
		require.NoError(t, err)
		require.Equal(t, want, applied)
	}
	active := func(at time.Time) []provider.ActivePlanModelRestriction {
		values, err := store.ActivePlanModelRestrictions(ctx, p.ID, "X", at)
		require.NoError(t, err)
		return values
	}
	a, b := newPlan(), newPlan()
	first := grade(a, 2, -time.Hour, "success", true)
	second := grade(b, 1, -time.Hour, "success", true)
	deadline := now.Add(time.Hour)
	second.ExpiresAt = &deadline
	apply(first, true)
	apply(second, true)
	require.Len(t, active(now), 2)
	apply(first, false)
	apply(grade(a, 1, -time.Minute, "success", false), false)
	apply(grade(a, 3, -2*time.Hour, "success", false), false)
	apply(grade(a, 2, -time.Minute, "failed", false), false)
	missing := first
	missing.ResultID = -1
	apply(missing, false)
	require.Len(t, active(now), 2)
	// Release the contenders together. The later run must dominate regardless
	// of the order in which PostgreSQL takes the per-owner row lock.
	earlier := grade(a, 2, -10*time.Minute, "success", true)
	later := grade(a, 2, -5*time.Minute, "success", false)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, v := range []provider.PlanModelVerdict{earlier, later} {
		workers.Go(func() {
			<-start
			_, err := store.ApplyPlanModelVerdict(ctx, v)
			if err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	workers.Wait()
	require.Len(t, active(now), 1)
	require.Equal(t, b, active(now)[0].PlanID)
	require.Empty(t, active(deadline))
	other, err := store.ActivePlanModelRestrictions(ctx, p.ID, "Y", now)
	require.NoError(t, err)
	require.Empty(t, other)
	_, err = db.ExecContext(ctx, "DELETE FROM scheduled_test_results WHERE plan_id IN ($1,$2)", a, b)
	require.NoError(t, err)
	require.Len(t, active(now), 1)
	var auditCount int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM provider_plan_model_verdicts WHERE provider_id=$1", p.ID).Scan(&auditCount))
	require.Equal(t, 6, auditCount)
	updated, err := client.Provider.Get(ctx, p.ID)
	require.NoError(t, err)
	require.False(t, updated.Schedulable)
}
