//go:build integration

package billing_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing/capability"
)

// settlement_contention_scenario_integration_test.go exercises funds.go,
// postgres/settlement.go, and postgres/usage_dedup_retention.go on the migrated
// billing database. Each worker owns its command so the public entry point
// can normalize the fingerprint and amounts independently. Persisted balances
// and Key counters are read as decimal text to catch differences at the
// smallest stored monetary unit.
//
// A deferred constraint trigger gives the failure case a checkpoint after
// the settlement statements reach commit. One attempt is rejected by the
// database and another loses its session while waiting at that checkpoint.
// An independent pool observes rollback and the settlement pool must obtain
// a new session before retrying the original financial identity.

// TestFundsSettleConcurrentIdentity races equal and conflicting fingerprints
// for the same financial identity. Exactly one attempt charges the wallet
// and Key, and the stored winner determines every other result.
func TestFundsSettleConcurrentIdentity(t *testing.T) {
	ctx := t.Context()
	payerID := newContentionPayer(t)
	keyID := newContentionKey(t, payerID)
	funds := billing.NewFunds(newSettlementFixture(integrationDB))
	requestID := uuid.NewString()
	amounts := [2]float64{0.125, 0.375}
	balances := [2]string{"4.87500000", "4.62500000"}
	keyUsages := [2]string{"0.12500000", "0.37500000"}

	// Eight callers per fingerprint exercise matching and conflicting waiters.
	commands := make([]billing.UsageBillingCommand, 16)
	for i := range commands {
		commands[i] = contentionWalletCommand(payerID, keyID, requestID, amounts[i%len(amounts)])
	}
	outcomes := settleConcurrently(t, funds, commands)

	var applied []int
	for i, outcome := range outcomes {
		if outcome.result != nil && outcome.result.Applied {
			applied = append(applied, i)
		}
	}
	require.Len(t, applied, 1, "applied indices: %v", applied)
	winner := applied[0]
	winnerVariant := winner % len(amounts)
	for i, outcome := range outcomes {
		switch {
		case i == winner:
			require.NoError(t, outcome.err)
			require.Equal(t, amounts[winnerVariant], outcome.result.BalanceAmountUSD)
			require.True(t, outcome.result.APIKeyQuotaExhausted)
		case i%len(amounts) == winnerVariant:
			require.NoError(t, outcome.err)
			require.Equal(t, &billing.UsageBillingApplyResult{}, outcome.result)
		default:
			require.ErrorIs(t, outcome.err, billing.ErrUsageBillingRequestConflict)
			require.Nil(t, outcome.result)
		}
	}

	assertContentionBalance(t, ctx, payerID, balances[winnerVariant])
	assertContentionKey(t, ctx, keyID, keyUsages[winnerVariant], apikey.StatusAPIKeyQuotaExhausted)
	fingerprint := commands[winner].RequestFingerprint
	require.Len(t, fingerprint, 64)
	assertContentionClaim(t, ctx, keyID, requestID, fingerprint, "")
}

// TestFundsSettleConcurrentPayer submits distinct actions against one wallet
// through one or several Keys. Shared action IDs across different Keys remain
// independent, and every half-tie charge appears in the wallet and its Key.
func TestFundsSettleConcurrentPayer(t *testing.T) {
	for _, tc := range []struct {
		name       string   // subtest display name
		rounds     int      // action identifiers per key
		keyUsage   []string // expected amount per key
		newBalance string   // expected payer wallet
	}{
		{
			name:       "one_key",
			rounds:     24,
			keyUsage:   []string{"0.24000024"},
			newBalance: "4.75999976",
		},
		{
			name:       "four_keys",
			rounds:     8,
			keyUsage:   []string{"0.08000008", "0.16000008", "0.24000008", "0.32000008"},
			newBalance: "4.19999968",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			payerID := newContentionPayer(t)
			funds := billing.NewFunds(newSettlementFixture(integrationDB))
			keyIDs := make([]int64, len(tc.keyUsage))
			for i := range keyIDs {
				keyIDs[i] = newContentionKey(t, payerID)
			}
			rawBase := [4]float64{0.02000001, 0.04000001, 0.06000001, 0.08000001}
			roundedCharge := [4]float64{0.01000001, 0.02000001, 0.03000001, 0.04000001}

			commands := make([]billing.UsageBillingCommand, 0, tc.rounds*len(keyIDs))
			for range tc.rounds {
				requestID := uuid.NewString()
				for keyIndex, keyID := range keyIDs {
					// The wallet rate makes each charge land on the ninth-decimal
					// midpoint. Each Key counter uses the actual rounded wallet charge.
					cmd := contentionWalletCommand(payerID, keyID, requestID, rawBase[keyIndex])
					cmd.BaseAmountUSD = rawBase[keyIndex]
					cmd.BalanceRateMultiplier = 0.5
					commands = append(commands, cmd)
				}
			}
			for i, outcome := range settleConcurrently(t, funds, commands) {
				cmd := commands[i]
				require.NoError(t, outcome.err, "request %s, key %d", cmd.RequestID, cmd.APIKeyID)
				require.NotNil(t, outcome.result)
				require.True(t, outcome.result.Applied, "request %s, key %d", cmd.RequestID, cmd.APIKeyID)
				require.Equal(t, roundedCharge[i%len(keyIDs)], outcome.result.BalanceAmountUSD)
				require.Len(t, cmd.RequestFingerprint, 64)
				assertContentionClaim(t, ctx, cmd.APIKeyID, cmd.RequestID, cmd.RequestFingerprint, "")
			}
			assertContentionBalance(t, ctx, payerID, tc.newBalance)
			for i, keyID := range keyIDs {
				status := apikey.StatusAPIKeyQuotaExhausted
				if len(keyIDs) > 1 && i == 0 {
					status = billing.StatusActive
				}
				assertContentionKey(t, ctx, keyID, tc.keyUsage[i], status)
			}
		})
	}
}

// TestFundsSettleCommitRecovery rejects a settlement at commit and interrupts
// another commit by terminating its session. Both attempts roll back before
// the same financial identity succeeds once through a replacement connection.
func TestFundsSettleCommitRecovery(t *testing.T) {
	ctx := t.Context()
	payerID := newContentionPayer(t)
	keyID := newContentionKey(t, payerID)
	cmd := contentionWalletCommand(payerID, keyID, uuid.NewString(), 0.125)
	providerRecord := mustCreateProvider(t, testEntClient(t), &provider.Record{
		Name: "billing-contention-" + uuid.NewString(),
		Type: capability.ProviderTypeAPIKey,
		Extra: map[string]any{
			"quota_limit":         0.10,
			"quota_daily_limit":   0.10,
			"quota_weekly_limit":  0.10,
			"quota_monthly_limit": 0.10,
		},
	})
	cmd.ProviderID = providerRecord.ID
	cmd.ProviderType = capability.ProviderTypeAPIKey
	// The ninth decimal rounds the provider charge up independently of the wallet.
	cmd.ProviderQuotaCost = 0.250000005
	installContentionCommitFault(t, payerID)

	retryPool := newContentionPool(t)
	funds := billing.NewFunds(newSettlementFixture(retryPool))

	rejected := cmd
	result, err := funds.Settle(ctx, &rejected)
	var pgErr *pq.Error
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, pq.ErrorCode("23514"), pgErr.Code)
	require.Equal(t, "billing_contention_commit_fault", pgErr.Constraint)
	require.Nil(t, result)
	assertContentionBalance(t, ctx, payerID, "5.00000000")
	assertContentionKey(t, ctx, keyID, "0.00000000", billing.StatusActive)
	assertContentionClaim(t, ctx, keyID, cmd.RequestID, "", "")
	assertContentionProvider(t, ctx, providerRecord.ID, "0.00000000", 0)

	var backendPID int
	require.NoError(t, retryPool.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&backendPID))
	interruptContentionCommit(t, funds, cmd, backendPID)
	assertContentionBalance(t, ctx, payerID, "5.00000000")
	assertContentionKey(t, ctx, keyID, "0.00000000", billing.StatusActive)
	assertContentionClaim(t, ctx, keyID, cmd.RequestID, "", "")
	assertContentionProvider(t, ctx, providerRecord.ID, "0.00000000", 0)
	removeContentionCommitFault(t)

	// Submit the original facts through the same pool; the terminated session
	// must be replaced and its rolled-back identity must remain available.
	retried := cmd
	result, err = funds.Settle(ctx, &retried)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Applied)
	require.True(t, result.APIKeyQuotaExhausted)
	require.Equal(t, 0.125, result.BalanceAmountUSD)
	require.NotNil(t, result.QuotaState)
	require.Equal(t, 0.25000001, result.QuotaState.TotalUsed)
	var replacementPID int
	require.NoError(t, retryPool.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&replacementPID))
	require.NotEqual(t, backendPID, replacementPID)

	replay, err := funds.Settle(ctx, &cmd)
	require.NoError(t, err)
	require.Equal(t, &billing.UsageBillingApplyResult{}, replay)
	require.Len(t, retried.RequestFingerprint, 64)
	require.Equal(t, rejected.RequestFingerprint, retried.RequestFingerprint)
	assertContentionBalance(t, ctx, payerID, "4.87500000")
	assertContentionKey(t, ctx, keyID, "0.12500000", apikey.StatusAPIKeyQuotaExhausted)
	assertContentionClaim(t, ctx, keyID, cmd.RequestID, retried.RequestFingerprint, "")
	assertContentionProvider(t, ctx, providerRecord.ID, "0.25000001", 1)
}

// TestFundsSettleArchivedIdentity moves a committed claim through the real
// archival job and races matching and conflicting copies. The archived claim
// stays authoritative, leaving the live table and funding totals unchanged.
func TestFundsSettleArchivedIdentity(t *testing.T) {
	ctx := t.Context()
	payerID := newContentionPayer(t)
	keyID := newContentionKey(t, payerID)
	funds := billing.NewFunds(newSettlementFixture(integrationDB))
	seed := contentionWalletCommand(payerID, keyID, uuid.NewString(), 0.125)
	settled := seed
	result, err := funds.Settle(ctx, &settled)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Applied)
	require.Len(t, settled.RequestFingerprint, 64)
	assertContentionClaim(t, ctx, keyID, seed.RequestID, settled.RequestFingerprint, "")

	cutoff := ageContentionClaim(t, seed)
	require.NoError(t, newAggregationFixture(integrationDB).CleanupUsageBillingDedup(ctx, cutoff))
	assertContentionClaim(t, ctx, keyID, seed.RequestID, "", settled.RequestFingerprint)

	conflict := seed
	conflict.RequestPayloadHash = "different-payload"
	for i, outcome := range settleConcurrently(t, funds, []billing.UsageBillingCommand{seed, conflict, seed, conflict}) {
		if i%2 == 0 {
			require.NoError(t, outcome.err)
			require.Equal(t, &billing.UsageBillingApplyResult{}, outcome.result)
		} else {
			require.ErrorIs(t, outcome.err, billing.ErrUsageBillingRequestConflict)
			require.Nil(t, outcome.result)
		}
	}
	assertContentionBalance(t, ctx, payerID, "4.87500000")
	assertContentionKey(t, ctx, keyID, "0.12500000", apikey.StatusAPIKeyQuotaExhausted)
	assertContentionClaim(t, ctx, keyID, seed.RequestID, "", settled.RequestFingerprint)
}

// TestFundsSettleDuringArchival starts replay while the retention job holds
// the deleted live claim in an uncommitted transaction. After the insert
// unblocks, settlement must see the committed archive before charging.
func TestFundsSettleDuringArchival(t *testing.T) {
	for _, tc := range []struct {
		name         string // subtest display name
		withKeyUsage bool   // enables Key counter writes
		conflict     bool   // changes replay payload identity
	}{
		{name: "counted_matching", withKeyUsage: true},
		{name: "counted_conflicting", withKeyUsage: true, conflict: true},
		{name: "pure_matching"},
		{name: "pure_conflicting", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runFundsSettleDuringArchival(t, tc.withKeyUsage, tc.conflict)
		})
	}
}

// runFundsSettleDuringArchival exercises one replay while its live claim
// is moving to the archive. The committed wallet and Key must retain the
// first charge with or without Key counters enabled.
func runFundsSettleDuringArchival(t *testing.T, withKeyUsage, conflict bool) {
	t.Helper()
	ctx := t.Context()
	payerID := newContentionPayer(t)
	var keyID int64
	if withKeyUsage {
		keyID = newContentionKey(t, payerID)
	} else {
		keyID = mustCreateApiKey(t, testEntClient(t), &apikey.APIKey{
			UserID: payerID, Key: "sk-archive-" + uuid.NewString(),
		}).ID
	}
	seed := contentionWalletCommand(payerID, keyID, uuid.NewString(), 0.125)
	keyUsage, keyStatus := "0.12500000", apikey.StatusAPIKeyQuotaExhausted
	if !withKeyUsage {
		seed.APIKeyQuotaCost = 0
		seed.APIKeyRateLimitCost = 0
		keyUsage, keyStatus = "0.00000000", billing.StatusActive
	}
	pool := newContentionPool(t)
	funds := billing.NewFunds(newSettlementFixture(pool))
	settled := seed
	result, err := funds.Settle(ctx, &settled)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Applied)
	require.Len(t, settled.RequestFingerprint, 64)
	cutoff := ageContentionClaim(t, seed)

	replay := seed
	if conflict {
		replay.RequestPayloadHash = "different-payload"
	}
	outcome := settleWhileArchiving(t, pool, funds, replay, cutoff)
	if conflict {
		require.ErrorIs(t, outcome.err, billing.ErrUsageBillingRequestConflict)
		require.Nil(t, outcome.result)
	} else {
		require.NoError(t, outcome.err)
		require.Equal(t, &billing.UsageBillingApplyResult{}, outcome.result)
	}
	assertContentionBalance(t, ctx, payerID, "4.87500000")
	assertContentionKey(t, ctx, keyID, keyUsage, keyStatus)
	assertContentionClaim(t, ctx, keyID, seed.RequestID, "", settled.RequestFingerprint)
}

// newContentionKey persists an active Key with a ten-cent total cap. All
// spending windows start at the fixture time so a batch near UTC midnight
// measures concurrent accumulation without a scheduled reset.
func newContentionKey(t *testing.T, payerID int64) int64 {
	t.Helper()
	now := time.Now()
	return mustCreateApiKey(t, testEntClient(t), &apikey.APIKey{
		UserID:        payerID,
		Key:           "sk-contention-" + uuid.NewString(),
		Quota:         0.10,
		Window5hStart: &now,
		Window1dStart: &now,
		Window7dStart: &now,
	}).ID
}

// newContentionPool opens an independent pool with at most one session.
// Tests can identify the backend before its next settlement, and test
// cleanup closes the pool without changing the shared fixture's connections.
func newContentionPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := sql.Open("postgres", integrationPostgresDSN)
	require.NoError(t, err)
	// Limit the pool to one open session so a captured backend PID identifies
	// the next settlement while the connection remains healthy.
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	return pool
}

// contentionWalletCommand requests a balance charge with both Key writes
// enabled. The caller receives raw monetary facts and a blank fingerprint
// so each newly submitted copy exercises actual settlement normalization.
func contentionWalletCommand(payerID, keyID int64, requestID string, amount float64) billing.UsageBillingCommand {
	return billing.UsageBillingCommand{
		RequestID:           requestID,
		UserID:              payerID,
		APIKeyID:            keyID,
		APIKeyBillingMode:   billing.APIKeyBillingModeBalance,
		BillableAmountUSD:   amount,
		APIKeyQuotaCost:     amount,
		APIKeyRateLimitCost: amount,
	}
}

// interruptContentionCommit terminates the caller's settlement session after
// the deferred trigger waits on the test's advisory lock. The caller supplies
// the session PID and exclusively owns a single-connection settlement pool.
//
// The test keeps the lock through termination, so releasing the lock cannot
// let the check violation decide this attempt's outcome. The worker finishes
// before returning, allowing the caller to inspect the database after rollback.
func interruptContentionCommit(t *testing.T, funds *billing.Funds, cmd billing.UsageBillingCommand, backendPID int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	blocker, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	var workers sync.WaitGroup
	// On failure, release the lock and cancel before joining so a blocked commit
	// cannot keep the test alive. On success the worker has already returned.
	defer func() {
		err := blocker.Rollback()
		cancel()
		workers.Wait()
		if err != sql.ErrTxDone {
			require.NoError(t, err)
		}
	}()
	_, err = blocker.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", -cmd.UserID)
	require.NoError(t, err)
	var blockerPID int
	require.NoError(t, blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))

	var outcome usageBillingApplyOutcome
	// Settle on a separate worker so the test can terminate it while the
	// database still holds the session at the deferred trigger.
	workers.Go(func() {
		outcome.result, outcome.err = funds.Settle(ctx, &cmd)
	})
	// Poll for actual lock contention at commit so a fast scheduler or slow
	// connection setup cannot cause termination before the writes are staged.
	waitForContentionBlock(t, ctx, backendPID, blockerPID)
	var terminated bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT pg_terminate_backend($1, 5000)", backendPID).Scan(&terminated))
	require.True(t, terminated)
	workers.Wait()
	require.Error(t, outcome.err)
	require.Nil(t, outcome.result)
}

// settleWhileArchiving runs real retention and settlement in separate
// transactions. Retention moves the live claim while its transaction stays
// open. The competing insert must wait for the deleting transaction, which
// fixes the ordering regardless of goroutine scheduling.
//
// The observer must report the lock wait before the test commits retention.
// Settlement then needs a fresh snapshot to observe the archived fingerprint.
// A failure in the setup releases the blocker before joining the worker.
func settleWhileArchiving(t *testing.T, pool *sql.DB, funds *billing.Funds, replay billing.UsageBillingCommand, cutoff time.Time) usageBillingApplyOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var replayPID, archivePID int
	require.NoError(t, pool.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&replayPID))
	archive, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	var workers sync.WaitGroup
	// Release any uncommitted deletion before draining the worker on failure,
	// so cleanup cannot hang on the unique index wait it set up.
	defer func() {
		err := archive.Rollback()
		cancel()
		workers.Wait()
		if err != sql.ErrTxDone {
			require.NoError(t, err)
		}
	}()
	require.NoError(t, archive.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&archivePID))
	require.NoError(t, newAggregationFixture(archive).CleanupUsageBillingDedup(ctx, cutoff))
	var outcome usageBillingApplyOutcome
	// Submit the competing request before committing retention so its first
	// statement can observe the transaction that deleted the original live row.
	workers.Go(func() {
		outcome.result, outcome.err = funds.Settle(ctx, &replay)
	})
	waitForContentionBlock(t, ctx, replayPID, archivePID)
	// Publish the archive row only after the replay is waiting on the live
	// claim. A snapshot taken before this commit cannot see that archive row.
	require.NoError(t, archive.Commit())
	workers.Wait()
	return outcome
}

// waitForContentionBlock confirms the intended PostgreSQL lock dependency.
// The observer borrows the shared pool and waits up to ten seconds for its
// view of the waiting backend to include the supplied blocker.
func waitForContentionBlock(t *testing.T, ctx context.Context, backendPID, blockerPID int) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		var blocked bool
		err := integrationDB.QueryRowContext(ctx,
			"SELECT $1 = ANY(pg_blocking_pids($2))", blockerPID, backendPID).Scan(&blocked)
		require.NoError(c, err)
		require.True(c, blocked, "settlement session %d is waiting on session %d", backendPID, blockerPID)
	}, 10*time.Second, 20*time.Millisecond)
}
