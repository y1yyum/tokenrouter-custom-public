//go:build integration

package billing_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbent "github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/batchimage"
	batchpostgres "github.com/TokenFlux/TokenRouter/internal/batchimage/postgres"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	billingpostgres "github.com/TokenFlux/TokenRouter/internal/billing/postgres"
	"github.com/TokenFlux/TokenRouter/internal/creative"
	creativepostgres "github.com/TokenFlux/TokenRouter/internal/creative/postgres"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/pkg/timezone"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/scheduler"
	schedulerpostgres "github.com/TokenFlux/TokenRouter/internal/scheduler/postgres"
)

// Billing scenarios use a package-wide migrated PostgreSQL database for
// committed rows and transaction-bound Ent clients for rollback suites. Fixture
// constructors fill selected defaults and persist domain relationships through
// the supplied client, so callers choose whether writes commit or belong to an
// outer transaction. Shared user-table truncation requires committed cases to
// finish their work before cleanup.
//
// User creation writes allowed-group rows after obtaining the stored user ID,
// and plan creation writes group mappings after the plan row is saved. Those
// helpers fail the test on any step and leave transaction completion to their
// caller. Billing user conversion copies notification-email entries while
// retaining the supplied slices and pointers for other selected fields, which
// matters when fixtures are later mutated.

// subscriptionContractEmptyGroups supplies absent routing-group detail for
// subscription entitlement fixtures.
type subscriptionContractEmptyGroups struct{}

// billingUsersForContract adapts identity repository results into selected
// billing user summaries.
type billingUsersForContract struct {
	repository identity.UserRepository // borrowed user lookup repository
}

// committedEntitlementClient returns the package-shared Ent client and
// registers cascading user-table truncation. Callers finish their workers
// before cleanup and serialize test lifetimes because truncation affects every
// user fixture in this database.
func committedEntitlementClient(t *testing.T) *dbent.Client {
	t.Helper()
	// Clear all users and their dependent rows after a committed case,
	// including reset IDs. The shared database requires other committed
	// fixtures to have ended.
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(context.Background(), "TRUNCATE users RESTART IDENTITY CASCADE")
		require.NoError(t, err)
	})
	return integrationEntClient
}

// entitlementTx starts an Ent transaction on the shared PostgreSQL client and
// registers rollback cleanup. Callers use its client for fixture writes and own
// any earlier commit or rollback.
func entitlementTx(t *testing.T) *dbent.Tx {
	t.Helper()
	tx, err := integrationEntClient.Tx(context.Background())
	require.NoError(t, err)
	// Roll back case-owned writes at test completion; discarding an
	// already-completed transaction error lets callers perform their
	// asserted commit or rollback earlier.
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// billingUserForContract copies selected user values into a billing summary and
// returns nil for nil input. Notification-email entries get a new slice; other
// copied slices and pointer fields retain their source storage.
func billingUserForContract(u *identity.User) *billing.UserSummary {
	if u == nil {
		return nil
	}
	out := &billing.UserSummary{
		ID:                         u.ID,
		Email:                      u.Email,
		Username:                   u.Username,
		Role:                       u.Role,
		Balance:                    u.Balance,
		FrozenBalance:              u.FrozenBalance,
		Concurrency:                u.Concurrency,
		Status:                     u.Status,
		AllowedGroups:              u.AllowedGroups,
		DisabledPublicGroups:       u.DisabledPublicGroups,
		LastActiveAt:               u.LastActiveAt,
		CreatedAt:                  u.CreatedAt,
		UpdatedAt:                  u.UpdatedAt,
		BalanceNotifyEnabled:       u.BalanceNotifyEnabled,
		BalanceNotifyThresholdType: u.BalanceNotifyThresholdType,
		BalanceNotifyThreshold:     u.BalanceNotifyThreshold,
		TotalRecharged:             u.TotalRecharged,
		RPMLimit:                   u.RPMLimit,
		APIKeyLimit:                u.APIKeyLimit,
		DeletedAt:                  u.DeletedAt,
	}
	// Allocate notification-email storage for the summary so mutating its
	// entries cannot modify the source user; other pointer and slice fields
	// remain shared.
	if u.BalanceNotifyExtraEmails != nil {
		out.BalanceNotifyExtraEmails = make([]billing.NotifyEmailSummary, len(u.BalanceNotifyExtraEmails))
		copy(out.BalanceNotifyExtraEmails, u.BalanceNotifyExtraEmails)
	}
	return out
}

// GetByIDLite supplies no group details for every supplied ID. It reports
// success with a nil value so these entitlement cases can construct services
// without querying routing-group fixtures.
func (subscriptionContractEmptyGroups) GetByIDLite(context.Context, int64) (*billing.SubscriptionPlanGroup, error) {
	return nil, nil
}

// newSettlementFixture constructs settlement storage using the supplied SQL
// pool and the live process-local calendar.
func newSettlementFixture(db *sql.DB) *billingpostgres.SettlementStore {
	return newSettlementFixtureWithCalendar(db, timezone.NewCalendar(time.Local))
}

// newSettlementFixtureWithCalendar constructs settlement storage using the
// supplied SQL pool and calendar. Tests can share the calendar with their
// quota anchors so settlement evaluates both against the same date.
func newSettlementFixtureWithCalendar(db *sql.DB, calendar timezone.Calendar) *billingpostgres.SettlementStore {
	return billingpostgres.NewSettlementStore(db, calendar, schedulerpostgres.EnqueueProviderQuotaChangedInTx, billingpostgres.TaskProjectionFactories{
		creative.FundingScope: func(tx *sql.Tx, ref billing.TaskReference) billingpostgres.TaskProjection {
			return creativepostgres.NewFundingParticipant(tx, ref.ID)
		},
		batchimage.FundingScope: func(tx *sql.Tx, ref billing.TaskReference) billingpostgres.TaskProjection {
			return batchpostgres.NewFundingParticipant(tx, ref.ID)
		},
	})
}

// testEntClient returns the package-shared Ent client after marking the caller
// as a test helper. The package runner owns its lifetime, and cases use
// transactions or their own row cleanup.
func testEntClient(t *testing.T) *dbent.Client { t.Helper(); return integrationEntClient }

// GetByID delegates user lookup and converts the returned user to selected
// billing fields. It returns the delegate error unchanged, with
// billingUserForContract determining nil handling and shared nested storage.
func (r billingUsersForContract) GetByID(ctx context.Context, id int64) (*billing.UserSummary, error) {
	user, err := r.repository.GetByID(ctx, id)
	return billingUserForContract(user), err
}

// uniqueTeamTestEmail returns a team-prefixed fixture email containing the
// supplied prefix and a fresh UUID.
func uniqueTeamTestEmail(prefix string) string {
	return fmt.Sprintf("team-%s-%s@example.com", prefix, uuid.NewString())
}

// mustCreateUser persists selected user fields, then fills its ID and stored
// timestamps on the supplied object. Empty email, password, role, and status
// receive fixture values, and zero concurrency becomes five.
//
// Allowed-group rows are inserted afterward using the new user ID. The supplied
// client determines whether these writes share a caller transaction; a later
// mapping failure fails the test after earlier writes have occurred. Callers
// retain transaction or committed-fixture cleanup ownership, and this helper
// uses a background context.
func mustCreateUser(t *testing.T, client *dbent.Client, u *identity.User) *identity.User {
	t.Helper()
	ctx := context.Background()

	if u.Email == "" {
		u.Email = "user-" + time.Now().Format(time.RFC3339Nano) + "@example.com"
	}
	if u.PasswordHash == "" {
		u.PasswordHash = "test-password-hash"
	}
	if u.Role == "" {
		u.Role = identity.RoleUser
	}
	if u.Status == "" {
		u.Status = billing.StatusActive
	}
	if u.Concurrency == 0 {
		u.Concurrency = 5
	}

	create := client.User.Create().
		SetEmail(u.Email).
		SetPasswordHash(u.PasswordHash).
		SetRole(u.Role).
		SetStatus(u.Status).
		SetBalance(u.Balance).
		SetConcurrency(u.Concurrency).
		SetUsername(u.Username).
		SetNotes(u.Notes)
	if !u.CreatedAt.IsZero() {
		create.SetCreatedAt(u.CreatedAt)
	}
	if !u.UpdatedAt.IsZero() {
		create.SetUpdatedAt(u.UpdatedAt)
	}

	created, err := create.Save(ctx)
	require.NoError(t, err, "create user")

	u.ID = created.ID
	u.CreatedAt = created.CreatedAt
	u.UpdatedAt = created.UpdatedAt

	// Persist group memberships after obtaining the user ID. The supplied
	// client controls their common transaction, and partial committed
	// fixtures require caller cleanup.
	if len(u.AllowedGroups) > 0 {
		for _, groupID := range u.AllowedGroups {
			_, err := client.UserAllowedGroup.Create().
				SetUserID(u.ID).
				SetGroupID(groupID).
				Save(ctx)
			require.NoError(t, err, "create user_allowed_groups row")
		}
	}

	return u
}

// mustCreateGroup persists selected routing fields and writes the stored ID and
// timestamps into g. Empty status becomes active; caller-supplied timestamps
// are used when present, and failures fail the test.
func mustCreateGroup(t *testing.T, client *dbent.Client, g *routing.Group) *routing.Group {
	t.Helper()
	ctx := context.Background()

	if g.Status == "" {
		g.Status = billing.StatusActive
	}

	create := client.Group.Create().
		SetName(g.Name).
		SetStatus(g.Status).
		SetRateMultiplier(g.RateMultiplier).
		SetIsExclusive(g.IsExclusive).
		SetForceOpenaiFast(g.ForceOpenAIFast)
	if g.Description != "" {
		create.SetDescription(g.Description)
	}
	if !g.CreatedAt.IsZero() {
		create.SetCreatedAt(g.CreatedAt)
	}
	if !g.UpdatedAt.IsZero() {
		create.SetUpdatedAt(g.UpdatedAt)
	}

	created, err := create.Save(ctx)
	require.NoError(t, err, "create group")

	g.ID = created.ID
	g.CreatedAt = created.CreatedAt
	g.UpdatedAt = created.UpdatedAt
	return g
}

// mustCreatePlan persists a subscription plan and its selected group mappings,
// then fills stored identity and timestamps on p. Zero validity selects thirty
// days and empty validity unit selects day.
//
// Each mapping follows plan insertion so it can reference the stored plan ID.
// Positive group multipliers are stored, while absent or nonpositive values
// produce a null mapping multiplier. Caller-supplied clients determine
// transaction scope, and a mapping failure can follow earlier successful writes
// before the test fails.
func mustCreatePlan(t *testing.T, client *dbent.Client, p *billing.SubscriptionPlan) *billing.SubscriptionPlan {
	t.Helper()
	ctx := context.Background()

	if p.ValidityDays == 0 {
		p.ValidityDays = 30
	}
	if p.ValidityUnit == "" {
		p.ValidityUnit = "day"
	}

	create := client.SubscriptionPlan.Create().
		SetName(p.Name).
		SetDescription(p.Description).
		SetPrice(p.Price).
		SetValidityDays(p.ValidityDays).
		SetValidityUnit(p.ValidityUnit).
		SetFeatures(p.Features).
		SetProductName(p.ProductName).
		SetForSale(p.ForSale).
		SetSortOrder(p.SortOrder)
	if p.OriginalPrice != nil {
		create.SetOriginalPrice(*p.OriginalPrice)
	}
	if p.DailyLimitUSD != nil {
		create.SetDailyLimitUsd(*p.DailyLimitUSD)
	}
	if p.WeeklyLimitUSD != nil {
		create.SetWeeklyLimitUsd(*p.WeeklyLimitUSD)
	}
	if p.MonthlyLimitUSD != nil {
		create.SetMonthlyLimitUsd(*p.MonthlyLimitUSD)
	}
	if p.GroupIDs != nil {
		create.SetGroupIds(p.GroupIDs)
	}
	if !p.CreatedAt.IsZero() {
		create.SetCreatedAt(p.CreatedAt)
	}
	if !p.UpdatedAt.IsZero() {
		create.SetUpdatedAt(p.UpdatedAt)
	}

	created, err := create.Save(ctx)
	require.NoError(t, err, "create subscription plan")
	// Write group mapping rows after the plan is saved so they reference
	// its storage ID; repeated mappings update the multiplier in the
	// supplied client scope.
	for _, groupID := range p.GroupIDs {
		var rate any
		if p.GroupRateMultipliers != nil {
			if value, ok := p.GroupRateMultipliers[groupID]; ok && value > 0 {
				rate = value
			}
		}
		_, err := client.ExecContext(ctx, `
			INSERT INTO subscription_plan_groups (plan_id, group_id, rate_multiplier)
			VALUES ($1, $2, $3)
			ON CONFLICT (plan_id, group_id)
			DO UPDATE SET rate_multiplier = EXCLUDED.rate_multiplier
		`, created.ID, groupID, rate)
		require.NoError(t, err, "create subscription plan group mapping")
	}

	p.ID = created.ID
	p.CreatedAt = created.CreatedAt
	p.UpdatedAt = created.UpdatedAt
	return p
}

// mustCreateApiKey persists selected credential, quota, spending-window, group,
// team, and timestamp fields and fills k's stored identity. Empty status,
// credential, and name receive fixture defaults; the caller supplies storage
// scope and owns cleanup. A nonnil GroupID is stored on the key, while funding
// commands can independently name their requested group.
func mustCreateApiKey(t *testing.T, client *dbent.Client, k *apikey.APIKey) *apikey.APIKey {
	t.Helper()
	ctx := context.Background()

	if k.Status == "" {
		k.Status = billing.StatusActive
	}
	if k.Key == "" {
		k.Key = "sk-" + time.Now().Format("150405.000000")
	}
	if k.Name == "" {
		k.Name = "default"
	}

	create := client.APIKey.Create().
		SetUserID(k.UserID).
		SetKey(k.Key).
		SetName(k.Name).
		SetStatus(k.Status)
	if k.Quota != 0 {
		create.SetQuota(k.Quota)
	}
	if k.QuotaUsed != 0 {
		create.SetQuotaUsed(k.QuotaUsed)
	}
	if k.RateLimit5h != 0 {
		create.SetRateLimit5h(k.RateLimit5h)
	}
	if k.RateLimit1d != 0 {
		create.SetRateLimit1d(k.RateLimit1d)
	}
	if k.RateLimit7d != 0 {
		create.SetRateLimit7d(k.RateLimit7d)
	}
	if k.Usage5h != 0 {
		create.SetUsage5h(k.Usage5h)
	}
	if k.Usage1d != 0 {
		create.SetUsage1d(k.Usage1d)
	}
	if k.Usage7d != 0 {
		create.SetUsage7d(k.Usage7d)
	}
	if k.Window5hStart != nil {
		create.SetWindow5hStart(*k.Window5hStart)
	}
	if k.Window1dStart != nil {
		create.SetWindow1dStart(*k.Window1dStart)
	}
	if k.Window7dStart != nil {
		create.SetWindow7dStart(*k.Window7dStart)
	}
	if k.ExpiresAt != nil {
		create.SetExpiresAt(*k.ExpiresAt)
	}
	if k.GroupID != nil {
		create.SetGroupID(*k.GroupID)
	}
	if k.TeamID != nil {
		create.SetTeamID(*k.TeamID)
	}
	if !k.CreatedAt.IsZero() {
		create.SetCreatedAt(k.CreatedAt)
	}
	if !k.UpdatedAt.IsZero() {
		create.SetUpdatedAt(k.UpdatedAt)
	}

	created, err := create.Save(ctx)
	require.NoError(t, err, "create api key")

	k.ID = created.ID
	k.CreatedAt = created.CreatedAt
	k.UpdatedAt = created.UpdatedAt
	return k
}

// mustCreateSubscription persists selected subscription funding and window
// fields and fills stored identity on s. Empty status becomes active, zero
// dates receive current-time defaults, and the caller supplies its transaction
// or committed client.
func mustCreateSubscription(t *testing.T, client *dbent.Client, s *billing.UserSubscription) *billing.UserSubscription {
	t.Helper()
	ctx := context.Background()

	if s.Status == "" {
		s.Status = billing.SubscriptionStatusActive
	}
	now := time.Now()
	if s.StartsAt.IsZero() {
		s.StartsAt = now.Add(-1 * time.Hour)
	}
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = now.Add(24 * time.Hour)
	}
	if s.AssignedAt.IsZero() {
		s.AssignedAt = now
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = now
	}

	create := client.UserSubscription.Create().
		SetUserID(s.UserID).
		SetPlanID(s.PlanID).
		SetStartsAt(s.StartsAt).
		SetExpiresAt(s.ExpiresAt).
		SetStatus(s.Status).
		SetAssignedAt(s.AssignedAt).
		SetNotes(s.Notes).
		SetDailyUsageUsd(s.DailyUsageUSD).
		SetWeeklyUsageUsd(s.WeeklyUsageUSD).
		SetMonthlyUsageUsd(s.MonthlyUsageUSD)

	if s.AssignedBy != nil {
		create.SetAssignedBy(*s.AssignedBy)
	}
	if s.DailyWindowStart != nil {
		create.SetDailyWindowStart(*s.DailyWindowStart)
	}
	if s.WeeklyWindowStart != nil {
		create.SetWeeklyWindowStart(*s.WeeklyWindowStart)
	}
	if s.MonthlyWindowStart != nil {
		create.SetMonthlyWindowStart(*s.MonthlyWindowStart)
	}
	if s.DailyLimitUSD != nil {
		create.SetDailyLimitUsd(*s.DailyLimitUSD)
	}
	if s.WeeklyLimitUSD != nil {
		create.SetWeeklyLimitUsd(*s.WeeklyLimitUSD)
	}
	if s.MonthlyLimitUSD != nil {
		create.SetMonthlyLimitUsd(*s.MonthlyLimitUSD)
	}
	if s.SourceOrderID != nil {
		create.SetSourceOrderID(*s.SourceOrderID)
	}
	if !s.CreatedAt.IsZero() {
		create.SetCreatedAt(s.CreatedAt)
	}
	if !s.UpdatedAt.IsZero() {
		create.SetUpdatedAt(s.UpdatedAt)
	}

	created, err := create.Save(ctx)
	require.NoError(t, err, "create user subscription")

	s.ID = created.ID
	s.CreatedAt = created.CreatedAt
	s.UpdatedAt = created.UpdatedAt
	return s
}

// float64Ptr returns a pointer to a distinct copy of the supplied
// floating-point fixture value.
func float64Ptr(v float64) *float64 {
	return &v
}

// settleConcurrently releases callers together once every worker has reached
// the start gate. The commands slice transfers exclusive access to each
// worker until the batch joins, making the normalized fingerprints available
// to the caller for ledger assertions.
//
// Each worker writes to its own result slot. The join provides synchronization
// for reading both commands and results and completes before any fixture
// cleanup can run. A shared deadline bounds database lock waits.
func settleConcurrently(t *testing.T, funds *billing.Funds, commands []billing.UsageBillingCommand) []usageBillingApplyOutcome {
	t.Helper()
	// Bound lock waits for the batch while allowing all workers to report errors.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	outcomes := make([]usageBillingApplyOutcome, len(commands))
	start := make(chan struct{})
	var ready, workers sync.WaitGroup
	ready.Add(len(commands))
	for i := range commands {
		// Wait until every caller is ready to contend. Each worker owns a
		// separate command because settlement normalizes the command in place.
		workers.Go(func() {
			ready.Done()
			<-start
			outcomes[i].result, outcomes[i].err = funds.Settle(ctx, &commands[i])
		})
	}
	ready.Wait()
	close(start)
	workers.Wait()
	return outcomes
}

// newContentionPayer persists a payer with five dollars in the billing fixture.
func newContentionPayer(t *testing.T) int64 {
	t.Helper()
	return mustCreateUser(t, testEntClient(t), &identity.User{
		Email:   "contention-" + uuid.NewString() + "@example.com",
		Balance: 5,
	}).ID
}

// assertContentionBalance checks the persisted wallet to all eight decimal places.
func assertContentionBalance(t *testing.T, ctx context.Context, payerID int64, want string) {
	t.Helper()
	var balance string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT balance::text FROM users WHERE id = $1", payerID).Scan(&balance))
	require.Equal(t, want, balance)
}

// assertContentionKey checks exact Key counter values and the persisted status.
func assertContentionKey(t *testing.T, ctx context.Context, keyID int64, wantAmount, wantStatus string) {
	t.Helper()
	var amounts [4]string
	var status string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT quota_used::text, usage_5h::text, usage_1d::text, usage_7d::text, status
		FROM api_keys WHERE id = $1
	`, keyID).Scan(&amounts[0], &amounts[1], &amounts[2], &amounts[3], &status))
	require.Equal(t, [4]string{wantAmount, wantAmount, wantAmount, wantAmount}, amounts, "key %d", keyID)
	require.Equal(t, wantStatus, status, "key %d", keyID)
}

// assertContentionClaim checks each table for a selected financial identity.
// An empty expected fingerprint represents an absent row; the fixtures only
// persist generated SHA-256 fingerprints.
func assertContentionClaim(t *testing.T, ctx context.Context, keyID int64, requestID, wantLive, wantArchived string) {
	t.Helper()
	var live, archived sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT
			(SELECT request_fingerprint FROM usage_billing_dedup WHERE request_id = $1 AND api_key_id = $2),
			(SELECT request_fingerprint FROM usage_billing_dedup_archive WHERE request_id = $1 AND api_key_id = $2)
	`, requestID, keyID).Scan(&live, &archived))
	require.Equal(t, sql.NullString{String: wantLive, Valid: wantLive != ""}, live, "live claim for %s/%d", requestID, keyID)
	require.Equal(t, sql.NullString{String: wantArchived, Valid: wantArchived != ""}, archived, "archive claim for %s/%d", requestID, keyID)
}

// assertContentionProvider checks provider counters and quota crossing events in storage.
func assertContentionProvider(t *testing.T, ctx context.Context, providerID int64, wantAmount string, wantEvents int) {
	t.Helper()
	var amounts [4]string
	var events int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COALESCE(extra->>'quota_used', '0')::numeric(20,8)::text,
		       COALESCE(extra->>'quota_daily_used', '0')::numeric(20,8)::text,
		       COALESCE(extra->>'quota_weekly_used', '0')::numeric(20,8)::text,
		       COALESCE(extra->>'quota_monthly_used', '0')::numeric(20,8)::text,
		       (SELECT COUNT(*) FROM scheduler_outbox WHERE provider_id = $1 AND event_type = $2)
		FROM providers WHERE id = $1
	`, providerID, scheduler.SchedulerOutboxEventProviderChanged).Scan(&amounts[0], &amounts[1], &amounts[2], &amounts[3], &events))
	require.Equal(t, [4]string{wantAmount, wantAmount, wantAmount, wantAmount}, amounts)
	require.Equal(t, wantEvents, events)
}

// ageContentionClaim moves a committed claim past the cutoff it returns.
// The test owns the supplied identity and uses the cutoff with the real
// retention job while the other recently created claims remain live.
func ageContentionClaim(t *testing.T, cmd billing.UsageBillingCommand) time.Time {
	t.Helper()
	cutoff := time.Now().UTC().AddDate(0, 0, -365)
	// Set the fixture timestamp before running retention so the archive receives
	// the fingerprint originally saved by settlement for this identity.
	_, err := integrationDB.ExecContext(t.Context(), `
		UPDATE usage_billing_dedup SET created_at = $1 WHERE request_id = $2 AND api_key_id = $3
	`, cutoff.Add(-time.Hour), cmd.RequestID, cmd.APIKeyID)
	require.NoError(t, err)
	return cutoff
}

// installContentionCommitFault defers a payer-scoped constraint to transaction
// commit. The trigger either raises a named check violation or waits for the
// test's advisory lock, which permits interrupting a specific database session.
func installContentionCommitFault(t *testing.T, payerID int64) {
	t.Helper()
	// Register teardown before DDL so a partial setup or failed assertion cannot
	// leave the fault installed for the next case in the package database.
	t.Cleanup(func() { removeContentionCommitFault(t) })
	_, err := integrationDB.ExecContext(t.Context(), fmt.Sprintf(`
		CREATE FUNCTION billing_contention_commit_fault() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(-NEW.id);
			RAISE EXCEPTION USING ERRCODE = '23514', CONSTRAINT = 'billing_contention_commit_fault',
				MESSAGE = 'injected billing commit failure';
		END;
		$$;
		CREATE CONSTRAINT TRIGGER billing_contention_commit_fault
		AFTER UPDATE ON users DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
		WHEN (OLD.id = %d AND OLD.balance IS DISTINCT FROM NEW.balance)
		EXECUTE FUNCTION billing_contention_commit_fault();
	`, payerID))
	require.NoError(t, err)
}

// removeContentionCommitFault clears test DDL and can run again during cleanup.
// It uses an independent deadline because the test context is canceled before
// cleanup functions run.
func removeContentionCommitFault(t *testing.T) {
	t.Helper()
	// Give DDL removal a fresh context so canceled settlement calls cannot leave
	// the payer-specific trigger active for other tests.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := integrationDB.ExecContext(ctx, `
		DROP TRIGGER IF EXISTS billing_contention_commit_fault ON users;
		DROP FUNCTION IF EXISTS billing_contention_commit_fault();
	`)
	require.NoError(t, err)
}
