//go:build integration

package postgres

import (
	"context"
	"fmt"
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
		return provider.PlanModelVerdict{
			ResultID: resultID, PlanID: plan, ProviderID: p.ID, PolicyVersion: version,
			RequestedModel: "client-x", UpstreamModel: "X", Restricted: restricted, Reason: "fixture grade",
		}
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

// TestScheduledQualityCommit injects failures on both sides of the commit.
// Rollback removes the ungraded input too; retention failures preserve the
// exact result ID and its decision. Plan revisions fence in-flight results.
func TestScheduledQualityCommit(t *testing.T) {
	db := postgrescontainer.New(t)
	ctx := t.Context()
	client := ent.NewClient(ent.Driver(sql.OpenDB(dialect.Postgres, db)))
	p, err := client.Provider.Create().SetName("P1").SetPlatform("openai").SetType("apikey").SetCredentials(map[string]any{}).SetSchedulable(false).Save(ctx)
	require.NoError(t, err)
	plans := NewScheduledTestPlanRepository(db)
	results := NewScheduledTestResultRepository(db)
	store := NewProviderStore(client, db, ProviderStoreOptions{Events: planVerdictEvents{}})
	service := provider.NewScheduledTestService(plans, results, provider.ScheduledTestOptions{SaveQuality: func(ctx context.Context, result *provider.ScheduledTestResult) (*provider.ScheduledTestResult, error) {
		return store.SaveQualityResult(ctx, db, result)
	}})
	plan, err := plans.Create(ctx, &provider.ScheduledTestPlan{ProviderID: p.ID, ModelID: "alias", Enabled: true, MaxResults: 1, QualityEnabled: true, CronExpression: "* * * * *"})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)
	observe := func(n int, grade string) *provider.ScheduledTestResult {
		at := now.Add(time.Duration(n) * time.Second)
		return &provider.ScheduledTestResult{
			Status: "success", StartedAt: at, FinishedAt: at, ResponseText: fmt.Sprint(n),
			Quality: &provider.ScheduledQualityObservation{ProviderID: p.ID, RequestedModel: "alias", UpstreamModel: "X", Revision: plan.QualityRevision, Version: 1, Rule: "arithmetic-array-v1", Grade: grade, Reason: "calibration"},
		}
	}
	count := func(table string) int {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n))
		return n
	}
	exec := func(query string) { t.Helper(); _, err := db.ExecContext(ctx, query); require.NoError(t, err) }
	first := observe(0, "fail")
	// Reject the initial insert, then force a later error during outbox publication.
	exec("ALTER TABLE scheduled_test_results ADD CHECK (response_text <> '0')")
	stored, err := service.SaveResult(ctx, plan.ID, 1, first)
	require.Error(t, err)
	require.Nil(t, stored)
	exec("ALTER TABLE scheduler_outbox ADD CONSTRAINT fail_event CHECK (false) NOT VALID")
	first.ResponseText = "sample"
	stored, err = service.SaveResult(ctx, plan.ID, 1, first)
	require.Error(t, err)
	require.Nil(t, stored)
	for _, table := range []string{"scheduled_test_results", "provider_plan_model_verdicts", "provider_plan_model_state", "scheduler_outbox"} {
		require.Zero(t, count(table), table)
	}
	exec("ALTER TABLE scheduler_outbox DROP CONSTRAINT fail_event")
	stored, err = service.SaveResult(ctx, plan.ID, 1, first)
	require.NoError(t, err)
	require.Positive(t, stored.ID)
	originalID := stored.ID
	// Pruning runs after commit. A trigger rejects retention of the older sample.
	exec("CREATE FUNCTION reject_prune() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'retention failure'; END $$")
	exec("CREATE TRIGGER reject_prune BEFORE DELETE ON scheduled_test_results FOR EACH ROW EXECUTE FUNCTION reject_prune()")
	stored, err = service.SaveResult(ctx, plan.ID, 1, observe(1, "pass"))
	require.Error(t, err)
	require.NotNil(t, stored)
	require.Greater(t, stored.ID, originalID)
	applied, err := store.ApplyPlanModelVerdict(ctx, provider.PlanModelVerdict{ResultID: stored.ID, PlanID: plan.ID, ProviderID: p.ID, RequestedModel: "alias", UpstreamModel: "X", PolicyVersion: 1, Reason: "replay"})
	require.NoError(t, err)
	require.False(t, applied)
	require.Equal(t, 2, count("scheduler_outbox"))
	exec("DROP TRIGGER reject_prune ON scheduled_test_results")
	require.NoError(t, results.PruneOldResults(ctx, plan.ID, 1))
	require.Equal(t, 1, count("scheduled_test_results"))
	require.Equal(t, 2, count("provider_plan_model_verdicts"))
	// Concurrent saves get distinct IDs and preserve the later start watermark.
	a, b := observe(2, "pass"), observe(3, "fail")
	type save struct {
		id   int64
		text string
		err  error
	}
	ch := make(chan save, 2)
	for _, row := range []*provider.ScheduledTestResult{a, b} {
		go func() {
			v, err := service.SaveResult(ctx, plan.ID, 10, row)
			if err != nil {
				ch <- save{err: err}
				return
			}
			ch <- save{id: v.ID, text: v.ResponseText}
		}()
	}
	saved := map[string]int64{}
	for range 2 {
		v := <-ch
		require.NoError(t, v.err)
		saved[v.text] = v.id
	}
	require.NotEqual(t, saved["2"], saved["3"])
	for text, id := range saved {
		var actual string
		require.NoError(t, db.QueryRowContext(ctx, "SELECT response_text FROM scheduled_test_results WHERE id=$1", id).Scan(&actual))
		require.Equal(t, text, actual)
	}
	active := func(at time.Time) []provider.ActivePlanModelRestriction {
		t.Helper()
		owners, err := store.ActivePlanModelRestrictions(ctx, p.ID, "X", at)
		require.NoError(t, err)
		return owners
	}
	require.Len(t, active(now), 1)
	require.Equal(t, saved["3"], active(now)[0].ResultID)
	stale := observe(4, "pass")
	plan.Enabled = false
	edited, err := plans.Update(ctx, plan)
	require.NoError(t, err)
	require.Greater(t, edited.QualityRevision, plan.QualityRevision)
	before := count("scheduler_outbox")
	stored, err = service.SaveResult(ctx, plan.ID, 10, stale)
	require.NoError(t, err)
	require.Equal(t, "plan_changed", stored.Quality.Reason)
	require.NoError(t, plans.UpdateAfterRun(ctx, plan.ID, plan.QualityRevision, now, now))
	current, err := plans.GetByID(ctx, plan.ID)
	require.NoError(t, err)
	require.Nil(t, current.LastRunAt)
	require.Len(t, active(now), 1)
	require.Empty(t, active(b.StartedAt.Add(provider.QualityRestrictionTTL)))
	require.Equal(t, before, count("scheduler_outbox"))
	edited.Enabled = true
	_, err = plans.Update(ctx, edited)
	require.NoError(t, err)
	stored, err = service.SaveResult(ctx, plan.ID, 10, stale)
	require.NoError(t, err)
	require.Equal(t, "ungraded", stored.Quality.Grade)
	require.Equal(t, before, count("scheduler_outbox"))
}
